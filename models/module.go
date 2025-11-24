package models

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cavaliergopher/grab/v3"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/utils"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	Updater           = resource.NewModel("viam", "windows_autoupdate", "updater")
	errNoUpdateNeeded = errors.New("no update needed")
)

func init() {
	resource.RegisterComponent(generic.API, Updater,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newWindowsAutoupdateUpdater,
		},
	)
}

type Config struct {
	DownloadURL            string   `json:"download_url"`
	DownloadDestination    string   `json:"download_destination"`
	InstallerPath          string   `json:"installer_path"`
	InstallArgs            []string `json:"install_args"`
	RegistryLookupKey      string   `json:"registry_lookup_key"`
	RegistryLookupValue    string   `json:"registry_lookup_value"`
	AbortOnUninstallErrors bool     `json:"abort_on_uninstall_errors"`
	ForceInstall           bool     `json:"force_install"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	_, err := url.Parse(cfg.DownloadURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid address '%s' for component at path '%s': %w", cfg.DownloadURL, path, err)
	}
	return nil, nil, nil
}

type windowsAutoupdateUpdater struct {
	name resource.Name

	logger logging.Logger
	cfg    *Config

	downloadWorkers  utils.StoppableWorkers
	downloadComplete bool

	resource.AlwaysRebuild
}

type cacheDetails struct {
	DownloadURL   string `json:"download_url"`
	ContentLength int64  `json:"content_length"`
	ETag          string `json:"etag"`
	Installed     bool   `json:"installed"`
}

func newWindowsAutoupdateUpdater(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	s := &windowsAutoupdateUpdater{
		name:             rawConf.ResourceName(),
		logger:           logger,
		cfg:              conf,
		downloadWorkers:  *utils.NewBackgroundStoppableWorkers(),
		downloadComplete: false,
	}

	s.downloadWorkers.Add(s.downloadIgnoringReturn)

	return s, nil
}

func (s *windowsAutoupdateUpdater) Name() resource.Name {
	return s.name
}

func (s *windowsAutoupdateUpdater) downloadIgnoringReturn(ctx context.Context) {
	// s.downloadComplete = false
	// s.downloadUpdate(ctx)
	s.downloadComplete = true
}

func (s *windowsAutoupdateUpdater) downloadUpdate(ctx context.Context) (string, error) {
	if !s.updateHasChanged(ctx) {
		s.logger.Infof("no update needed")
		return "", errNoUpdateNeeded
	}

	var destination string
	if s.cfg.DownloadDestination != "" {
		destination = s.cfg.DownloadDestination
	} else {
		var err error
		destination, err = s.getCacheDir()
		if err != nil {
			destination = os.TempDir()
		}
	}

	client := grab.NewClient()
	req, err := grab.NewRequest(destination, s.cfg.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("could not create request: %w", err)
	}
	req = req.WithContext(ctx)

	// start download
	s.logger.Infof("downloading update from: %v", req.URL())
	resp := client.Do(req)

	if freeSpace, err := getFreeDiskSpace(destination[:2]); err == nil {
		if freeSpace < uint64(resp.Size()*3) {
			resp.Cancel()
			return "", fmt.Errorf("not enough free space on drive %s: %d bytes available, %d bytes needed", destination[:2], freeSpace, resp.Size()*3)
		}
	}

	// start status loop
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()

Loop:
	for {
		select {
		case <-t.C:
			s.logger.Debugf("downloaded %v / %v bytes (%.2f%%)", resp.BytesComplete(), resp.Size(), 100*resp.Progress())
		case <-resp.Done:
			s.logger.Debugf("downloaded %v / %v bytes (%.2f%%)", resp.BytesComplete(), resp.Size(), 100*resp.Progress())
			break Loop
		}
	}

	// check for errors
	if err := resp.Err(); err != nil {
		return "", fmt.Errorf("could not download file: %w", err)
	}

	// save download details
	cacheDetails := cacheDetails{
		DownloadURL:   s.cfg.DownloadURL,
		ContentLength: resp.Size(),
		ETag:          resp.HTTPResponse.Header.Get("etag"),
		Installed:     false,
	}
	s.setCacheDetails(cacheDetails)

	// success
	s.logger.Infof("update saved to %s", resp.Filename)
	return resp.Filename, nil
}

func (s *windowsAutoupdateUpdater) updateHasChanged(ctx context.Context) bool {
	if s.cfg.ForceInstall {
		return true
	}

	cacheDetails := s.getCacheDetails()
	if cacheDetails.DownloadURL != s.cfg.DownloadURL {
		s.logger.Debugf("download URL has changed from %s to %s", cacheDetails.DownloadURL, s.cfg.DownloadURL)
		return true
	}
	resp, err := http.Head(s.cfg.DownloadURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		s.logger.Debugf("error getting head for %s: %v", s.cfg.DownloadURL, err)
		return true
	}
	if resp.ContentLength != cacheDetails.ContentLength {
		s.logger.Debugf("content length has changed from %d to %d", cacheDetails.ContentLength, resp.ContentLength)
		return true
	}
	if resp.Header.Get("etag") != cacheDetails.ETag {
		s.logger.Debugf("etag has changed from %s to %s", cacheDetails.ETag, resp.Header.Get("etag"))
		return true
	}
	if !cacheDetails.Installed {
		s.logger.Debug("update has not changed, but has not been installed yet")
		return true
	}
	return false
}

func (s *windowsAutoupdateUpdater) getCacheDir() (string, error) {
	cacheDir := filepath.Join(os.TempDir(), "viam", string(Updater.Family.Namespace), Updater.Family.Name, Updater.Name, s.name.Name)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}
	return cacheDir, nil
}

func (s *windowsAutoupdateUpdater) getCacheDetailsFile() (string, error) {
	cacheDir, err := s.getCacheDir()
	if err != nil {
		return "", err
	}
	cacheFile := filepath.Join(cacheDir, "cache.json")
	f, err := os.OpenFile(cacheFile, os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return cacheFile, nil
}

func (s *windowsAutoupdateUpdater) getCacheDetails() cacheDetails {
	cacheFile, err := s.getCacheDetailsFile()
	if err != nil {
		return cacheDetails{}
	}
	f, err := os.Open(cacheFile)
	if err != nil {
		return cacheDetails{}
	}
	defer f.Close()
	var details cacheDetails
	if err := json.NewDecoder(f).Decode(&details); err != nil {
		return cacheDetails{}
	}
	return details
}

func (s *windowsAutoupdateUpdater) setCacheDetails(details cacheDetails) error {
	cacheFile, err := s.getCacheDetailsFile()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(cacheFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(details); err != nil {
		return err
	}
	return nil
}

func getFreeDiskSpace(drive string) (uint64, error) {
	var freeBytesAvailable uint64
	var totalNumberOfBytes uint64
	var totalNumberOfFreeBytes uint64

	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(drive), &freeBytesAvailable, &totalNumberOfBytes, &totalNumberOfFreeBytes); err != nil {
		return 0, err
	}
	return freeBytesAvailable, nil
}

func unzipUpdate(src, dest string, logger logging.Logger) error {
	logger.Infof("unzipping %s to %s", src, dest)
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}

	extractAndWriteFile := func(f *zip.File) error {
		rc, err := f.Open()
		if err != nil {
			os.RemoveAll(dest)
			return err
		}
		defer rc.Close()

		p := filepath.Join(dest, f.Name)

		// Check for ZipSlip (Directory traversal)
		if !strings.HasPrefix(p, filepath.Clean(dest)+string(os.PathSeparator)) {
			os.RemoveAll(dest)
			return fmt.Errorf("illegal file path: %s", p)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(p, f.Mode())
		} else {
			os.MkdirAll(filepath.Dir(p), f.Mode())
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
			if err != nil {
				os.RemoveAll(dest)
				return err
			}
			defer f.Close()

			_, err = io.Copy(f, rc)
			if err != nil {
				os.RemoveAll(dest)
				return err
			}
		}
		return nil
	}

	for _, f := range r.File {
		err := extractAndWriteFile(f)
		if err != nil {
			return err
		}
	}

	return nil
}

// Find the installer in the downloaded update.
// If the downloaded update is a zip, unzip first.
// Search for something that looks like an installer.
// Return the installer, the (unzipped) directory if in a folder, and error
func (s *windowsAutoupdateUpdater) findInstaller(src string) (string, string, error) {

	extensions := []string{".exe", ".msi", ".bat"}

	if path.Ext(src) == ".zip" {
		s.logger.Info("update is a zip file, unzipping...")
		dest := strings.TrimSuffix(src, path.Ext(src))
		err := unzipUpdate(src, dest, s.logger)
		if err != nil {
			os.RemoveAll(dest)
			return "", "", err
		}
		src = dest
	}

	desc, err := os.Stat(src)
	if err != nil {
		return "", "", err
	}

	if desc.IsDir() {
		if s.cfg.InstallerPath != "" {
			installerPath := filepath.Join(src, s.cfg.InstallerPath)
			if _, err := os.Stat(installerPath); err != nil {
				os.RemoveAll(src)
				return "", "", fmt.Errorf("could not find installer at %s: %w", installerPath, err)
			}
			return installerPath, src, nil
		}

		files, err := os.ReadDir(src)
		if err != nil {
			os.RemoveAll(src)
			return "", "", err
		}
		for _, file := range files {
			if slices.Contains(extensions, path.Ext(file.Name())) {
				return filepath.Join(src, file.Name()), src, nil
			}
		}
	} else {
		if slices.Contains(extensions, path.Ext(src)) {
			return src, "", nil
		}
	}
	return "", "", errors.New("could not find a file that resembles an installer")
}

func (s *windowsAutoupdateUpdater) uninstallExistingInstallation() error {
	// Skip uninstall step if these config values are not provided
	if len(strings.TrimSpace(s.cfg.RegistryLookupKey)) <= 0 {
		s.logger.Info("Skipping uninstall: Registry lookup key was not provided.")
		return nil
	}
	if len(strings.TrimSpace(s.cfg.RegistryLookupValue)) <= 0 {
		s.logger.Info("Skipping uninstall: Registry lookup value was not provided.")
		return nil
	}

	s.logger.Infof("uninstalling program(s) with registry key %s: %s", s.cfg.RegistryLookupKey, s.cfg.RegistryLookupValue)
	keys := []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	}

	uninstallCount := 0
	errors := []error{}
	for _, key_name := range keys {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, key_name, registry.READ)
		if err != nil {
			s.logger.Debugf("error checking registry at %s: %w", key_name, err)
			continue
		}
		defer k.Close()

		subkeys, err := k.ReadSubKeyNames(0)
		if err != nil {
			s.logger.Debugf("error getting subkeys for %s: %w", key_name, err)
			continue
		}
		for _, subkey := range subkeys {
			s.logger.Debugf("checking registry key: %s\\%s", key_name, subkey)
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, fmt.Sprintf(`%s\%s`, key_name, subkey), registry.READ)
			if err != nil {
				s.logger.Debugf("error opening subkey %s: %w", subkey, err)
				continue
			}
			defer sk.Close()

			lookupValue, _, err := sk.GetStringValue(s.cfg.RegistryLookupKey)
			if err != nil {
				s.logger.Debugf("error getting value for key %s\\%s - %s: %w", key_name, subkey, s.cfg.RegistryLookupKey, err)
				continue
			}
			if lookupValue == s.cfg.RegistryLookupValue {
				script, _, err := sk.GetStringValue("QuietUninstallString")
				if err != nil || len(strings.TrimSpace(script)) <= 0 {
					script, _, err = sk.GetStringValue("UninstallString")
					if err != nil {
						errors = append(errors, fmt.Errorf("could not find uninstall command: %w", err))
						continue
					}
				}

				// Force quiet uninstall if using MsiExec
				if strings.Contains(script, "MsiExec.exe") {
					script += " /quiet"
				}

				fullCommand := fmt.Sprintf("cmd /C %s", script)
				s.logger.Infof("running uninstall command: %s", fullCommand)
				cmd := exec.Command(fullCommand)
				output, err := cmd.CombinedOutput()
				if err != nil {
					errors = append(errors, fmt.Errorf("encountered error uninstalling program: %s", string(output[:])))
					continue
				}
				uninstallCount++
				s.logger.Infof("successfully uninstalled: %s", string(output[:]))
			}
		}
	}
	if uninstallCount > 0 {
		s.logger.Infof("uninstalled %d programs", uninstallCount)
	} else if len(errors) > 0 {
		return fmt.Errorf("encountered errors uninstalling programs: %v", errors)
	} else {
		s.logger.Info("existing installation not found")
	}
	return nil
}

func (s *windowsAutoupdateUpdater) installUpdate(installer string) error {
	s.logger.Infof("installing update from %s", installer)
	args := append([]string{"/C", installer}, s.cfg.InstallArgs...)
	cmd := exec.Command("cmd", args...)
	s.logger.Infof("installation command: %s", args)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("encountered error installing program: %s", string(output[:]))
	}
	s.logger.Infof("successfully installed: %s", string(output[:]))
	return nil
}

func (s *windowsAutoupdateUpdater) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	for utils.SelectContextOrWait(ctx, 1*time.Second) {
		if s.downloadComplete {
			break
		}
		s.logger.Info("waiting for download to complete...")
	}
	update, err := s.downloadUpdate(ctx)
	if err != nil {
		return nil, err
	}
	defer os.Remove(update)

	// Check if installer exists before uninstalling anything
	installer, dir, err := s.findInstaller(update)
	if err != nil {
		return nil, err
	}
	defer func() {
		if dir != "" {
			os.RemoveAll(dir)
		} else {
			os.RemoveAll(installer)
		}
	}()

	if err := s.uninstallExistingInstallation(); err != nil && s.cfg.AbortOnUninstallErrors {
		return nil, err
	}

	if err := s.installUpdate(installer); err != nil {
		return nil, err
	}

	// Update cache details to indicate that the update has been installed
	cacheDetails := s.getCacheDetails()
	cacheDetails.Installed = true
	if err := s.setCacheDetails(cacheDetails); err != nil {
		s.logger.Errorf("error setting cache details: %v", err)
	}

	return nil, nil
}

func (s *windowsAutoupdateUpdater) Close(context.Context) error {
	// Put close code here
	s.downloadWorkers.Stop()
	return nil
}
