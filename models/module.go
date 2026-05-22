//go:build windows

package models

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unsafe"

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
	DownloadRetryCount     int      `json:"download_retry_count"`
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
	if !s.updateHasChanged() {
		s.logger.Infof("no update needed")
		return "", errNoUpdateNeeded
	}

	var destination string
	if s.cfg.DownloadDestination != "" {
		destination = s.cfg.DownloadDestination
		if err := os.MkdirAll(destination, 0755); err != nil {
			return "", err
		}
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

	var downloadErr error
	retryCount := s.cfg.DownloadRetryCount
	if retryCount == -1 {
		retryCount = math.MaxInt
	}
	for retries := 0; retries < retryCount; retries++ {
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
			downloadErr = fmt.Errorf("could not download file on attempt %d: %w", retries+1, err)
			continue
		}
		downloadErr = nil

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
	return "", downloadErr
}

func (s *windowsAutoupdateUpdater) updateHasChanged() bool {
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
				s.logger.Infof("Windows provided uninstall script: %s", script)

				exe, args, err := SplitExeAndArgs(script)
				if err != nil {
					// No exe was specified
					continue
				}
				s.logger.Infof("The exe is '%s' and the args are '%s'", exe, args)

				// Replace all occurrences of /quiet with an empty string. Force interactive uninstall mode
				args = strings.ReplaceAll(args, "/quiet", "")

				// Launch the uninstaller in the active user session, wait for it to finish
				fullCommand := fmt.Sprintf("%s %s", exe, args)
				s.logger.Infof("uninstallation command: %s", fullCommand)
				exitCode, err := LaunchInActiveUserSession(fullCommand, true)
				if exitCode != 0 {
					s.logger.Infof("uninstaller exit code: %d", exitCode)
				}
				if err != nil {
					s.logger.Fatalf("LaunchInActiveUserSession() uninstaller failed: %v", err)
				}
				s.logger.Infof("successfully uninstalled: %s via %s", s.cfg.RegistryLookupValue, exe)

				uninstallCount++
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

	if slices.Contains(s.cfg.InstallArgs, "/quiet") {
		// Run the install in the background thread, using the Go exec command
		args := append([]string{"/C", installer}, s.cfg.InstallArgs...)
		cmd := exec.Command("cmd", args...)
		s.logger.Infof("installation command: %s", args)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("encountered error installing program: %s", string(output[:]))
		}
		s.logger.Infof("successfully installed: %s", string(output[:]))
	} else {
		// Run the install in the foreground as an interactive install
		// Double double quotes is not a typo
		//  Use double double quotes to ensure cmd.exe parses the setup.exe correctly when the rest may contain quotes/spaces.
		//  eg cmd.exe /C ""<command with spaces>" and /args here"
		args := fmt.Sprintf(`C:\\windows\\system32\\cmd.exe /C ""%s" %s"`, installer, strings.Join(s.cfg.InstallArgs, " "))
		args = strings.ReplaceAll(args, "/quiet", "")
		s.logger.Infof("installation command: %s.", args)

		exitCode, err := LaunchInActiveUserSession(args, true)
		if exitCode != 0 {
			s.logger.Infof("installer exit code: %d", exitCode)
		}
		if err != nil {
			s.logger.Errorf("LaunchInActiveUserSession() installer failed: %v", err)
			return err
		}
		s.logger.Infof("successfully installed %s", s.cfg.RegistryLookupValue)
	}
	return nil
}

func (s *windowsAutoupdateUpdater) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	// Some of the config parameters can be overridden dynamically
	lookupkey, ok := cmd["registry_lookup_key"]
	if ok {
		defer func(origRegistryLookupKey string) {
			s.cfg.RegistryLookupKey = origRegistryLookupKey
		}(s.cfg.RegistryLookupKey)
		s.cfg.RegistryLookupKey = lookupkey.(string)
	}

	lookupvalue, ok := cmd["registry_lookup_value"]
	if ok {
		defer func(origRegistryLookupValue string) {
			s.cfg.RegistryLookupValue = origRegistryLookupValue
		}(s.cfg.RegistryLookupValue)
		s.cfg.RegistryLookupValue = lookupvalue.(string)
	}

	downloadurl, ok := cmd["download_url"]
	if ok {
		defer func(origDownloadURL string) {
			s.cfg.DownloadURL = origDownloadURL
		}(s.cfg.DownloadURL)
		s.cfg.DownloadURL = downloadurl.(string)
	}

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

// LaunchInActiveUserSession launches an executable with args in the active console user's session.
// wait: if true, waits for completion and returns the process exit code.
func LaunchInActiveUserSession(appPath string, wait bool) (uint32, error) {
	sessionID := windows.WTSGetActiveConsoleSessionId()
	if sessionID == 0xFFFFFFFF {
		return 0, fmt.Errorf("no active console session")
	}

	// 1) Get user token for the active session
	var userToken windows.Token
	if err := windows.WTSQueryUserToken(sessionID, &userToken); err != nil {
		return 0, fmt.Errorf("WTSQueryUserToken(session=%d): %w", sessionID, err)
	}
	defer userToken.Close()

	// 2) Duplicate token to PRIMARY token for CreateProcessAsUser
	var primaryToken windows.Token
	if err := windows.DuplicateTokenEx(
		userToken,
		windows.MAXIMUM_ALLOWED,
		nil,
		windows.SecurityImpersonation,
		windows.TokenPrimary,
		&primaryToken,
	); err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer primaryToken.Close()

	// 3) Build environment block (optional but recommended for "acts like a user" behavior)
	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, primaryToken, false); err != nil {
		// Not fatal; some environments block this. Proceed without it.
		env = nil
	} else {
		defer windows.DestroyEnvironmentBlock(env)
	}

	// 4) Prepare StartupInfo / ProcessInformation
	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop, _ = windows.UTF16PtrFromString("winsta0\\default")

	var pi windows.ProcessInformation
	defer func() {
		if pi.Thread != 0 {
			_ = windows.CloseHandle(pi.Thread)
		}
		if pi.Process != 0 {
			_ = windows.CloseHandle(pi.Process)
		}
	}()

	// 5) Build a CreateProcess-style command line.
	// For CreateProcessAsUser, lpCommandLine must be a mutable buffer.
	cmdLineUTF16, err := windows.UTF16FromString(appPath)
	if err != nil {
		return 0, fmt.Errorf("UTF16FromString(cmdLine): %w", err)
	}

	creationFlags := uint32(windows.CREATE_UNICODE_ENVIRONMENT)

	var dirPtr *uint16
	dirPtr, err = windows.UTF16PtrFromString(`C:\Windows\Temp`)

	// 6) Create the process as the user.
	if err := windows.CreateProcessAsUser(
		primaryToken,
		nil,
		&cmdLineUTF16[0],
		nil,
		nil,
		false,
		creationFlags,
		env, // can be nil
		dirPtr,
		&si,
		&pi,
	); err != nil {
		return 0, fmt.Errorf("CreateProcessAsUser: %w", err)
	}

	if !wait {
		return 0, nil
	}

	// 7) Wait and return exit code.
	_, werr := windows.WaitForSingleObject(pi.Process, windows.INFINITE)
	if werr != nil {
		return 0, fmt.Errorf("WaitForSingleObject: %w", werr)
	}

	var exitCode uint32
	if err := windows.GetExitCodeProcess(pi.Process, &exitCode); err != nil {
		return 0, fmt.Errorf("GetExitCodeProcess: %w", err)
	}

	return exitCode, nil
}
