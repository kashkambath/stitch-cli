package commands

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/10gen/stitch-cli/api"
	"github.com/10gen/stitch-cli/hosting"
	"github.com/10gen/stitch-cli/models"
	u "github.com/10gen/stitch-cli/user"
	"github.com/10gen/stitch-cli/utils"

	"github.com/mitchellh/cli"
	"github.com/mitchellh/go-homedir"
)

const (
	importFlagPath           = "path"
	importFlagStrategy       = "strategy"
	importFlagAppName        = "app-name"
	importFlagIncludeHosting = "include-hosting"
	importFlagResetCDNCache  = "reset-cdn-cache"
	importStrategyMerge      = "merge"
	importStrategyReplace    = "replace"
)

// Set of location and deployment model options supported by Stitch backend
var (
	locationOptions        = []string{"US-VA", "US-OR", "IE", "AU"}
	deploymentModelOptions = []string{"GLOBAL", "LOCAL"}
)

func errCreateAppSyncFailure(err error) error {
	return fmt.Errorf("failed to sync app with local directory after creation: %s", err)
}

func errImportAppSyncFailure(err error) error {
	return fmt.Errorf("failed to sync app with local directory after import: %s", err)
}

func errIncludeHosting(err error) error {
	return fmt.Errorf("--include-hosting error: %s", err)
}

// NewImportCommandFactory returns a new cli.CommandFactory given a cli.Ui
func NewImportCommandFactory(ui cli.Ui) cli.CommandFactory {
	return func() (cli.Command, error) {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return nil, err
		}

		return &ImportCommand{
			BaseCommand: &BaseCommand{
				Name: "import",
				UI:   ui,
			},
			workingDirectory: workingDirectory,
			writeToDirectory: utils.WriteZipToDir,
			writeAppConfigToFile: func(dest string, app models.AppInstanceData) error {
				return app.MarshalFile(dest)
			},
		}, nil
	}
}

// ImportCommand is used to import a Stitch App
type ImportCommand struct {
	*BaseCommand

	writeToDirectory     func(dest string, zipData io.Reader, overwrite bool) error
	writeAppConfigToFile func(dest string, app models.AppInstanceData) error
	workingDirectory     string

	flagAppID          string
	flagAppPath        string
	flagAppName        string
	flagGroupID        string
	flagStrategy       string
	flagIncludeHosting bool
	flagResetCDNCache  bool
}

// Help returns long-form help information for this command
func (ic *ImportCommand) Help() string {
	return `Import and deploy a stitch application from a local directory.

REQUIRED:
  --app-id [string]
	The App ID for your app (i.e. the name of your app followed by a unique suffix, like "my-app-nysja").

  --app-name [string]
	The name of your app to be used if app is to be created new.

OPTIONS:
  --path [string]
	A path to the local directory containing your app.

  --project-id [string]
	The Atlas Project ID.

  --strategy [merge|replace] (default: merge)
	How your app should be imported.	
	merge - import and overwrite existing entities while preserving those that exist on Stitch. Secrets missing will not be lost.
	replace - like merge but does not preserve entities missing from the local directory's app configuration.


  --include-hosting
	Upload static assets from "/hosting" directory.

  --reset-cdn-cache
	Invalidate cdn cache for modified files.	
	` +
		ic.BaseCommand.Help()
}

// Synopsis returns a one-liner description for this command
func (ic *ImportCommand) Synopsis() string {
	return `Import and deploy a stitch application from a local directory.`
}

// Run executes the command
func (ic *ImportCommand) Run(args []string) int {
	flags := ic.NewFlagSet()

	flags.StringVar(&ic.flagAppID, flagAppIDName, "", "")
	flags.StringVar(&ic.flagAppPath, importFlagPath, "", "")
	flags.StringVar(&ic.flagGroupID, flagProjectIDName, "", "")
	flags.StringVar(&ic.flagAppName, importFlagAppName, "", "")
	flags.StringVar(&ic.flagStrategy, importFlagStrategy, importStrategyMerge, "")
	flags.BoolVar(&ic.flagIncludeHosting, importFlagIncludeHosting, false, "")
	flags.BoolVar(&ic.flagResetCDNCache, importFlagResetCDNCache, false, "")

	if err := ic.BaseCommand.run(args); err != nil {
		ic.UI.Error(err.Error())
		return 1
	}

	if ic.flagStrategy != importStrategyMerge && ic.flagStrategy != importStrategyReplace {
		ic.UI.Error(fmt.Sprintf("unknown import strategy %q; accepted values are [%s|%s]", ic.flagStrategy, importStrategyMerge, importStrategyReplace))
		return 1
	}

	if err := ic.importApp(); err != nil {
		ic.UI.Error(err.Error())
		return 1
	}

	return 0
}

func (ic *ImportCommand) importApp() error {
	user, err := ic.User()
	if err != nil {
		return err
	}

	if !user.LoggedIn() {
		return u.ErrNotLoggedIn
	}

	appPath, err := ic.resolveAppDirectory()
	if err != nil {
		return err
	}

	appInstanceData, err := ic.resolveAppInstanceData(appPath)
	if err != nil {
		return err
	}

	loadedApp, err := utils.UnmarshalFromDir(appPath)
	if err != nil {
		return err
	}

	appData, err := json.Marshal(loadedApp)
	if err != nil {
		return err
	}

	stitchClient, err := ic.StitchClient()
	if err != nil {
		return err
	}

	app, err := ic.fetchAppByClientAppID(appInstanceData.AppID())
	var appNotFound bool
	if err != nil {
		switch err.(type) {
		case api.ErrAppNotFound:
			appNotFound = true
			if appInstanceData.AppID() == "" {
				err = errors.New("this app does not exist yet")
			}
		default:
			return err
		}
	}

	var skipDiff bool

	if appNotFound {
		skipDiff = true
		ic.flagStrategy = importStrategyReplace

		var wantedNewApp bool
		app, wantedNewApp, err = ic.askCreateEmptyApp(err.Error(), appInstanceData.AppName(), appInstanceData.AppLocation(), appInstanceData.AppDeploymentModel(), stitchClient)
		if err != nil {
			return err
		}
		if !wantedNewApp {
			return nil
		}

		appInstanceData[models.AppIDField] = app.ClientAppID
		appInstanceData[models.AppNameField] = app.Name

		if writeErr := ic.writeAppConfigToFile(appPath, appInstanceData); writeErr != nil {
			return errCreateAppSyncFailure(writeErr)
		}
	}

	var assetMetadataDiffs *hosting.AssetMetadataDiffs
	rootDir, dirErr := filepath.Abs(filepath.Join(appPath, utils.HostingFilesDirectory))
	if dirErr != nil {
		return dirErr
	}
	if ic.flagIncludeHosting {
		assetDescs, fileErr := hosting.MetadataFileToAssetDescriptions(filepath.Join(appPath, utils.HostingAttributes))
		if fileErr != nil {
			return errIncludeHosting(fmt.Errorf("error loading metadata.json file: %v", fileErr))
		}

		cachePath, cPErr := getAssetCachePath(ic.flagConfigPath)
		if cPErr != nil {
			return cPErr
		}

		assetCache, cErr := hosting.CacheFileToAssetCache(cachePath)
		if cErr != nil {
			if !os.IsNotExist(cErr) {
				return cErr
			}
			assetCache = hosting.NewAssetCache()
		}

		localAssetMetadata, aMErr :=
			hosting.ListLocalAssetMetadata(appInstanceData.AppID(), rootDir, assetDescs, assetCache)

		if aMErr != nil {
			return errIncludeHosting(fmt.Errorf("error processing local assets %s: %s", rootDir, aMErr))
		}

		if assetCache.Dirty() {
			if uError := hosting.UpdateCacheFile(cachePath, assetCache); uError != nil {
				ic.UI.Error(uError.Error())
			}
		}

		remoteAssetMetadata, rAMErr := stitchClient.ListAssetsForAppID(app.GroupID, app.ID)
		if rAMErr != nil {
			return errIncludeHosting(fmt.Errorf("error retrieving remote assets: %s", rAMErr))
		}

		assetMetadataDiffs = hosting.DiffAssetMetadata(localAssetMetadata, remoteAssetMetadata, ic.flagStrategy == importStrategyMerge)
	}

	// Diff changes unless -y flag has been provided or if this is a new app
	if !ic.flagYes && !skipDiff {
		diffs, diffErr := stitchClient.Diff(app.GroupID, app.ID, appData, ic.flagStrategy)

		if diffErr != nil {
			return fmt.Errorf("failed to diff app with currently deployed instance: %s", diffErr)
		}

		if ic.flagIncludeHosting && assetMetadataDiffs != nil {
			hostingDiff := assetMetadataDiffs.Diff()
			diffs = append(diffs, hostingDiff...)
		}

		if len(diffs) == 0 {
			ic.UI.Info("Deployed app is identical to proposed version, nothing to do.")
			return nil
		}

		for _, diff := range diffs {
			ic.UI.Info(diff)
		}

		confirm, askErr := ic.AskYesNo("Please confirm the changes shown above:")
		if askErr != nil {
			return askErr
		}

		if !confirm {
			return nil
		}
	}

	ic.UI.Info("Importing app...")
	if importErr := stitchClient.Import(app.GroupID, app.ID, appData, ic.flagStrategy); importErr != nil {
		return fmt.Errorf("failed to import app: %s", importErr)
	}
	ic.UI.Info("Done.")

	if ic.flagIncludeHosting && assetMetadataDiffs != nil {
		ic.UI.Info("Importing hosting assets...")
		if hostingImportErr := ImportHosting(app.GroupID, app.ID, rootDir, assetMetadataDiffs, ic.flagResetCDNCache, stitchClient, ic.UI); hostingImportErr != nil {
			return fmt.Errorf("failed to import hosting assets %s", hostingImportErr)
		}
		ic.UI.Info("Done.")
	}

	// re-fetch imported app to sync IDs
	_, body, err := stitchClient.Export(app.GroupID, app.ID, false)
	if err != nil {
		return errImportAppSyncFailure(err)
	}

	defer body.Close()

	if err := ic.writeToDirectory(appPath, body, true); err != nil {
		return errImportAppSyncFailure(err)
	}

	ic.UI.Info(fmt.Sprintf("Successfully imported '%s'", app.ClientAppID))

	return nil
}

func (ic *ImportCommand) fetchAppByClientAppID(clientAppID string) (*models.App, error) {
	stitchClient, err := ic.StitchClient()
	if err != nil {
		return nil, err
	}

	if ic.flagGroupID == "" {
		return stitchClient.FetchAppByClientAppID(clientAppID)
	}

	return stitchClient.FetchAppByGroupIDAndClientAppID(ic.flagGroupID, clientAppID)
}

func (ic *ImportCommand) resolveGroupID() (string, error) {
	if ic.flagGroupID != "" {
		return ic.flagGroupID, nil
	}

	atlasClient, err := ic.AtlasClient()
	if err != nil {
		return "", fmt.Errorf("an unexpected error occurred: %s", err)
	}

	groups, err := atlasClient.Groups()
	if err != nil {
		return "", err
	}

	groupsByName := map[string]string{}
	for _, group := range groups {
		groupsByName[group.Name] = group.ID
	}

	if len(groupsByName) == 0 {
		return "", errors.New("no available Projects")
	}

	ic.UI.Info("Available Projects:")

	for name, id := range groupsByName {
		ic.UI.Info(fmt.Sprintf("%s - %s", name, id))
	}

	var groupID string
	for {
		projectResponse, err := ic.Ask("Atlas Project Name or ID", groups[0].Name)
		if err != nil {
			return "", err
		}

		if isObjectIDHex(projectResponse) {
			groupID = projectResponse
			break
		}

		groupID = groupsByName[projectResponse]
		if groupID != "" {
			break
		}

		groupFromName, err := atlasClient.GroupByName(projectResponse)
		if err != nil {
			return "", err
		}

		groupID = groupFromName.ID
		if groupID != "" {
			break
		}

		ic.UI.Info("Could not understand response, please try again")
	}

	return groupID, nil
}

func (ic *ImportCommand) askCreateEmptyApp(query, defaultAppName, defaultLocation, defaultDeploymentModel string, stitchClient api.StitchClient) (*models.App, bool, error) {
	if ic.flagAppName != "" {
		defaultAppName = ic.flagAppName
	}

	confirm, err := ic.AskYesNo(fmt.Sprintf("%s: would you like to create a new app?", query))
	if err != nil {
		return nil, false, err
	}

	if !confirm {
		return nil, false, nil
	}

	appName, err := ic.Ask("App name", defaultAppName)
	if err != nil {
		return nil, false, err
	}

	groupID, err := ic.resolveGroupID()
	if err != nil {
		return nil, false, err
	}

	apps, err := stitchClient.FetchAppsByGroupID(groupID)
	if err != nil {
		return nil, false, err
	}

	for _, app := range apps {
		if app.Name == appName {
			return nil, false, fmt.Errorf("app already exists with name %q", appName)
		}
	}

	location, err := ic.AskWithOptions("Location", defaultLocation, locationOptions)
	if err != nil {
		return nil, false, err
	}

	deploymentModel, err := ic.AskWithOptions("Deployment Model", defaultDeploymentModel, deploymentModelOptions)
	if err != nil {
		return nil, false, err
	}

	app, err := stitchClient.CreateEmptyApp(groupID, appName, location, deploymentModel)
	if err != nil {
		return nil, false, err
	}

	ic.UI.Info(fmt.Sprintf("New app created: %s", app.ClientAppID))
	return app, true, nil
}

func (ic *ImportCommand) resolveAppDirectory() (string, error) {
	if ic.flagAppPath != "" {
		path, err := homedir.Expand(ic.flagAppPath)
		if err != nil {
			return "", err
		}

		if _, err := os.Stat(path); err != nil {
			return "", errors.New("directory does not exist")
		}
		return path, nil
	}

	return utils.GetDirectoryContainingFile(ic.workingDirectory, models.AppConfigFileName)
}

// resolveAppInstanceData loads data for an app from a stitch.json file located in the provided directory path,
// merging in any overridden parameters from command line flags
func (ic *ImportCommand) resolveAppInstanceData(path string) (models.AppInstanceData, error) {
	appInstanceDataFromFile := models.AppInstanceData{}
	err := appInstanceDataFromFile.UnmarshalFile(path)

	if os.IsNotExist(err) {
		return models.AppInstanceData{
			models.AppIDField: ic.flagAppID,
		}, nil
	}

	if err != nil {
		return nil, err
	}

	if ic.flagAppID != "" {
		appInstanceDataFromFile[models.AppIDField] = ic.flagAppID
	}

	return appInstanceDataFromFile, nil
}

// isObjectIDHex returns whether s is a valid hex representation of an ObjectId.
// copied from mgo/bson#IsObjectIdHex
func isObjectIDHex(s string) bool {
	if len(s) != 24 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
