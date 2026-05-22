# Windows Auto-Updater
### Update an application on a Windows machine

## Description
Update your Windows applications. Will try to
1. Download the update from the specified URL on (re)configuration
1. Uninstall an existing installation of the same program
1. Install the update

This module tries to do everything silently, but there might be UAC prompts if the module is not run in administrator mode.

## Attributes
The following attributes are available for this model:

| Name                        | Type     | Inclusion | Description                                                                       |
|-----------------------------|----------|-----------|-------------------------------------------------------------------|
| `download_url`              | string   | Required  | URL of the update                                                 |
| `download_destination`      | string   | Optional  | Alternate destination to store the download                       |
| `installer_path`            | string   | Optional  | Path of the installer                                             |
| `install_args`              | []string | Optional  | Any args to pass                                                  |
| `registry_lookup_key`       | string   | Optional  | Key for uninstaller                                               |
| `registry_lookup_value`     | string   | Optional  | Value for uninstaller                                             |
| `abort_on_uninstall_errors` | bool     | Optional  | Should the update should fail if uninstallation encounters errors |
| `force_install`             | bool     | Optional  | Install the update regardless of if the update has changed        |
| `download_retry_count`      | number   | Optional  | How many times to retry a failed download. -1 is infinite.        |

#### Example Configuration

```json
{
  "installer_path": "MyInstaller.exe",
  "install_args": [
    "/passive",
    "/norestart"
  ],
  "registry_lookup_key": "Publisher",
  "registry_lookup_value": "My Windows Publisher, Inc.",
  "download_url": "https://example.com/MyInstaller.zip",
  "abort_on_uninstall_errors": true,
  "force_install": false,
  "download_retry_count": 1,
}
```

### `download_url` 
**REQUIRED** 
The URL of the update

### `download_destination`
**OPTIONAL** default: `os.TempDir`

An alternate download directory. The default location of the download is the operating system's `temp` directory

### `installer_path` 
**OPTIONAL** default: `<empty string>`

The path of the installer if the download URL points to a directory. This value should be relative to the root of the downloaded directory. If the downloaded update is a zip, the module will unzip first.

This is optional because the module will search for an installer (extensions `.exe`, `.msi`, or `.bat`) in the downloaded directory, but ONLY ONE LEVEL DEEP. So if the zip file stores the installer in a nested directory, you should add the `installer_path` attribute.

For example, if the downloaded update has a directory structure as follows:
```
downloaded_directory/
├─ README.md
├─ Release-Notes.txt
├─ setup.exe
```
The module will find the `setup.exe` installer and run it.

However, if the downloaded update has the directory structure
```
downloaded_directory/
├─ subdirectory/
│  ├─ README.md
│  ├─ Release-Notes.txt
│  ├─ setup.exe
```
the module will NOT find the `setup.exe` installer. In this case, you should specify the `installer_path` attribute as `subdirectory\setup.exe`.

### `install_args` 
**OPTIONAL** default: `<empty array>`

Array of arguments to pass to the installer. For example, `["/quiet", "/norestart"]`. Note: not all installers support arguments.

### `registry_lookup_key`
**OPTIONAL** default: `<empty string>`

Used in conjunction with `registry_lookup_value` in order to identify any existing installations in the registry to uninstall them. The module will uninstall all applications found that match these parameters. If not provided, the module will skip the uninstall step.

For example, you could provide the `registry_lookup_key` of "DisplayName", with the associated `registry_lookup_value` of "Some Example Program" in order to uninstall "Some Example Program". If multiple registry items have the same registry key/value, the module will attempt to uninstall all of them.

### `registry_lookup_value`
**OPTIONAL** default: `<empty string>`

Used in conjunction with `registry_lookup_key` in order to identify any existing installations in the registry to uninstall them. The module will uninstall all applications found that match these parameters. If not provided, the module will skip the uninstall step.

For example, you could provide the `registry_lookup_key` of "DisplayName", with the associated `registry_lookup_value` of "Some Example Program" in order to uninstall "Some Example Program". If multiple registry items have the same registry key/value, the module will attempt to uninstall all of them.

### `abort_on_uninstall_errors`
**OPTIONAL** default: `false`

The module will first try to uninstall any existing installation (see `registry_lookup_key` and `registry_lookup_value`). Should the module encounter any errors, this flag will determine if the install process will abort or continue.

### `force_install`
**OPTIONAL** default: `false`

The module will not attempt to download or install the update if it detects that nothing has changed (criteria: same URL, same content size, same etag). Use this option to force the update to download and install, regardless the lack of changes.

### `download_retry_count`
**OPTIONAL** default: `0`

In the event of a failed download, the module can attempt to retry downloading the update. The default is `0`, meaning no retries. `-1` will make the module retry indefinitely until it succeeds. There is a 10sec pause between retry attempts.

## Usage
The module will begin to download the update immediately upon (re)configuration. Send an empty `doCommand` to this component to start the remainder of the update process.

The DoCommand can also be called with parameters to override the configuration attributes.

```json
{
  "download_url":"https://example.com/setup.zip",
  "registry_lookup_key":"DisplayName",
  "registry_lookup_value":"Some Example Program"
}
```
