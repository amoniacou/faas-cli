// Copyright (c) Alex Ellis 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package builder

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	v1execute "github.com/alexellis/go-execute/pkg/v1"
	"github.com/openfaas/faas-cli/schema"
	"github.com/openfaas/faas-cli/stack"
	vcs "github.com/openfaas/faas-cli/versioncontrol"
)

// AdditionalPackageBuildArg holds the special build-arg keyname for use with build-opts.
// Can also be passed as a build arg hence needs to be accessed from commands
const AdditionalPackageBuildArg = "ADDITIONAL_PACKAGE"

type BuildImageConfig struct {
	Image          string
	Handler        string
	FunctionName   string
	Language       string
	NoCache        bool
	Squash         bool
	ShrinkWrap     bool
	QuiteBuild     bool
	BuildArgMap    map[string]string
	BuildLabelMap  map[string]string
	BuildFlags     []string
	BuildOptions   []string
	CopyExtraPaths []string
	TagMode        schema.BuildFormat
}

// BuildImage construct Docker image from function parameters
// TODO: refactor signature to a struct to simplify the length of the method header
func BuildImage(config BuildImageConfig) error {

	if stack.IsValidTemplate(config.Language) {
		pathToTemplateYAML := fmt.Sprintf("./template/%s/template.yml", config.Language)
		if _, err := os.Stat(pathToTemplateYAML); os.IsNotExist(err) {
			return err
		}

		langTemplate, err := stack.ParseYAMLForLanguageTemplate(pathToTemplateYAML)
		if err != nil {
			return fmt.Errorf("error reading language template: %s", err.Error())
		}

		branch, version, err := GetImageTagValues(config.TagMode)
		if err != nil {
			return err
		}

		imageName := schema.BuildImageName(config.TagMode, config.Image, version, branch)

		if err := ensureHandlerPath(config.Handler); err != nil {
			return fmt.Errorf("building %s, %s is an invalid path", imageName, config.Handler)
		}

		tempPath, buildErr := createBuildContext(config.FunctionName, config.Handler, config.Language, isLanguageTemplate(config.Language), langTemplate.HandlerFolder, config.CopyExtraPaths)
		fmt.Printf("Building: %s with %s template. Please wait..\n", imageName, config.Language)
		if buildErr != nil {
			return buildErr
		}

		if config.ShrinkWrap {
			fmt.Printf("%s shrink-wrapped to %s\n", config.FunctionName, tempPath)
			return nil
		}

		buildOptPackages, buildPackageErr := getBuildOptionPackages(config.BuildOptions, config.Language, langTemplate.BuildOptions)

		if buildPackageErr != nil {
			return buildPackageErr

		}

		dockerBuildVal := dockerBuild{
			Image:            imageName,
			NoCache:          config.NoCache,
			Squash:           config.Squash,
			HTTPProxy:        os.Getenv("http_proxy"),
			HTTPSProxy:       os.Getenv("https_proxy"),
			BuildArgMap:      config.BuildArgMap,
			BuildOptPackages: buildOptPackages,
			BuildLabelMap:    config.BuildLabelMap,
			BuildFlags:       config.BuildFlags,
		}

		command, args := getDockerBuildCommand(dockerBuildVal)

		task := v1execute.ExecTask{
			Cwd:         tempPath,
			Command:     command,
			Args:        args,
			StreamStdio: !config.QuiteBuild,
		}

		res, err := task.Execute()

		if err != nil {
			return err
		}

		if res.ExitCode != 0 {
			return fmt.Errorf("[%s] received non-zero exit code from build, error: %s", config.FunctionName, res.Stderr)
		}

		fmt.Printf("Image: %s built.\n", imageName)

	} else {
		return fmt.Errorf("language template: %s not supported, build a custom Dockerfile", config.Language)
	}

	return nil
}

// GetImageTagValues returns the image tag format and component information determined via GIT
func GetImageTagValues(tagType schema.BuildFormat) (branch, version string, err error) {
	switch tagType {
	case schema.SHAFormat:
		version = vcs.GetGitSHA()
		if len(version) == 0 {
			err = fmt.Errorf("cannot tag image with Git SHA as this is not a Git repository")
			return
		}
	case schema.BranchAndSHAFormat:
		branch = vcs.GetGitBranch()
		if len(branch) == 0 {
			err = fmt.Errorf("cannot tag image with Git branch and SHA as this is not a Git repository")
			return

		}

		version = vcs.GetGitSHA()
		if len(version) == 0 {
			err = fmt.Errorf("cannot tag image with Git SHA as this is not a Git repository")
			return

		}
	case schema.DescribeFormat:
		version = vcs.GetGitDescribe()
		if len(version) == 0 {
			err = fmt.Errorf("cannot tag image with Git Tag and SHA as this is not a Git repository")
			return
		}
	}

	return branch, version, nil
}

func getDockerBuildCommand(build dockerBuild) (string, []string) {
	flagSlice := buildFlagSlice(build)
	args := []string{"build"}
	args = append(args, flagSlice...)

	args = append(args, "--tag", build.Image, ".")

	command := "docker"

	return command, args
}

type dockerBuild struct {
	Image            string
	Version          string
	NoCache          bool
	Squash           bool
	HTTPProxy        string
	HTTPSProxy       string
	BuildArgMap      map[string]string
	BuildOptPackages []string
	BuildLabelMap    map[string]string

	// Optional flags
	BuildFlags []string

	// Platforms for use with buildx and publish command
	Platforms string

	// ExtraTags for published images like :latest
	ExtraTags []string
}

var defaultDirPermissions os.FileMode = 0700

const defaultHandlerFolder string = "function"

// isRunningInCI checks the ENV var CI and returns true if it's set to true or 1
func isRunningInCI() bool {
	if env, ok := os.LookupEnv("CI"); ok {
		if env == "true" || env == "1" {
			return true
		}
	}
	return false
}

// createBuildContext creates temporary build folder to perform a Docker build with language template
func createBuildContext(functionName string, handler string, language string, useFunction bool, handlerFolder string, copyExtraPaths []string) (string, error) {
	tempPath := fmt.Sprintf("./build/%s/", functionName)
	fmt.Printf("Clearing temporary build folder: %s\n", tempPath)

	clearErr := os.RemoveAll(tempPath)
	if clearErr != nil {
		fmt.Printf("Error clearing temporary build folder: %s\n", tempPath)
		return tempPath, clearErr
	}

	functionPath := tempPath

	if useFunction {
		if handlerFolder == "" {
			functionPath = path.Join(functionPath, defaultHandlerFolder)
		} else {
			functionPath = path.Join(functionPath, handlerFolder)
		}
	}

	fmt.Printf("Preparing: %s %s\n", handler+"/", functionPath)

	if isRunningInCI() {
		defaultDirPermissions = 0777
	}

	mkdirErr := os.MkdirAll(functionPath, defaultDirPermissions)
	if mkdirErr != nil {
		fmt.Printf("Error creating path: %s - %s.\n", functionPath, mkdirErr.Error())
		return tempPath, mkdirErr
	}

	if useFunction {
		copyErr := CopyFiles(path.Join("./template/", language), tempPath)
		if copyErr != nil {
			fmt.Printf("Error copying template directory: %s.\n", copyErr.Error())
			return tempPath, copyErr
		}
	}

	// Overlay in user-function
	// CopyFiles(handler, functionPath)
	infos, readErr := ioutil.ReadDir(handler)
	if readErr != nil {
		fmt.Printf("Error reading the handler: %s - %s.\n", handler, readErr.Error())
		return tempPath, readErr
	}

	for _, info := range infos {
		switch info.Name() {
		case "build", "template":
			fmt.Printf("Skipping \"%s\" folder\n", info.Name())
			continue
		default:
			copyErr := CopyFiles(
				filepath.Clean(path.Join(handler, info.Name())),
				filepath.Clean(path.Join(functionPath, info.Name())),
			)

			if copyErr != nil {
				return tempPath, copyErr
			}
		}
	}

	for _, extraPath := range copyExtraPaths {
		extraPathAbs, err := pathInScope(extraPath, ".")
		if err != nil {
			return tempPath, err
		}
		// Note that if useFunction is false, ie is a `dockerfile` template, then
		// functionPath == tempPath, the docker build context, not the `function` handler folder
		// inside the docker build context
		copyErr := CopyFiles(
			extraPathAbs,
			filepath.Clean(path.Join(functionPath, extraPath)),
		)

		if copyErr != nil {
			return tempPath, copyErr
		}
	}

	return tempPath, nil
}

// pathInScope returns the absolute path to `path` and ensures that it is located within the
// provided scope. An error will be returned, if the path is outside of the provided scope.
func pathInScope(path string, scope string) (string, error) {
	scope, err := filepath.Abs(filepath.FromSlash(scope))
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(filepath.FromSlash(path))
	if err != nil {
		return "", err
	}

	if abs == scope {
		return "", fmt.Errorf("forbidden path appears to equal the entire project: %s (%s)", path, abs)
	}

	if strings.HasPrefix(abs, scope) {
		return abs, nil
	}

	// default return is an error
	return "", fmt.Errorf("forbidden path appears to be outside of the build context: %s (%s)", path, abs)
}

// appears to be unused???
func dockerBuildFolder(functionName string, handler string, language string) string {
	tempPath := fmt.Sprintf("./build/%s/", functionName)
	fmt.Printf("Clearing temporary build folder: %s\n", tempPath)

	clearErr := os.RemoveAll(tempPath)
	if clearErr != nil {
		fmt.Printf("Error clearing temporary build folder: %s\n", tempPath)
	}

	fmt.Printf("Preparing: %s %s\n", handler+"/", tempPath)

	// Both Dockerfile and dockerfile are accepted
	if language == "Dockerfile" {
		language = "dockerfile"
	}

	// CopyFiles(handler, tempPath)
	infos, readErr := ioutil.ReadDir(handler)
	if readErr != nil {
		fmt.Printf("Error reading the handler: %s - %s.\n", handler, readErr.Error())
	}

	for _, info := range infos {
		switch info.Name() {
		case "build", "template":
			fmt.Printf("Skipping \"%s\" folder\n", info.Name())
			continue
		default:
			copyErr := CopyFiles(
				filepath.Clean(path.Join(handler, info.Name())),
				filepath.Clean(path.Join(tempPath, info.Name())),
			)

			if copyErr != nil {
				log.Fatal(copyErr)
			}
		}
	}

	return tempPath
}

func buildFlagSlice(build dockerBuild) []string {

	var spaceSafeBuildFlags []string

	if build.NoCache {
		spaceSafeBuildFlags = append(spaceSafeBuildFlags, "--no-cache")
	}
	if build.Squash {
		spaceSafeBuildFlags = append(spaceSafeBuildFlags, "--squash")
	}

	if len(build.HTTPProxy) > 0 {
		spaceSafeBuildFlags = append(spaceSafeBuildFlags, "--build-arg", fmt.Sprintf("http_proxy=%s", build.HTTPProxy))
	}

	if len(build.HTTPSProxy) > 0 {
		spaceSafeBuildFlags = append(spaceSafeBuildFlags, "--build-arg", fmt.Sprintf("https_proxy=%s", build.HTTPSProxy))
	}

	for _, v := range build.BuildFlags {
		spaceSafeBuildFlags = append(spaceSafeBuildFlags, strings.Split(v, " ")...)
	}

	for k, v := range build.BuildArgMap {

		if k != AdditionalPackageBuildArg {
			spaceSafeBuildFlags = append(spaceSafeBuildFlags, "--build-arg", fmt.Sprintf("%s=%s", k, v))
		} else {
			build.BuildOptPackages = append(build.BuildOptPackages, strings.Split(v, " ")...)
		}
	}
	if len(build.BuildOptPackages) > 0 {
		build.BuildOptPackages = deDuplicate(build.BuildOptPackages)
		spaceSafeBuildFlags = append(spaceSafeBuildFlags, "--build-arg", fmt.Sprintf("%s=%s", AdditionalPackageBuildArg, strings.Join(build.BuildOptPackages, " ")))
	}

	for k, v := range build.BuildLabelMap {
		spaceSafeBuildFlags = append(spaceSafeBuildFlags, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	return spaceSafeBuildFlags
}

func ensureHandlerPath(handler string) error {
	if _, err := os.Stat(handler); err != nil {
		return err
	}

	return nil
}

func getBuildOptionPackages(requestedBuildOptions []string, language string, availableBuildOptions []stack.BuildOption) ([]string, error) {

	var buildPackages []string

	if len(requestedBuildOptions) > 0 {

		var allFound bool

		buildPackages, allFound = getPackages(availableBuildOptions, requestedBuildOptions)

		if !allFound {

			return nil, fmt.Errorf(
				`Error: You're using a build option unavailable for %s.
Please check /template/%s/template.yml for supported build options`, language, language)
		}

	}
	return buildPackages, nil
}

func getBuildOptionsFor(language string) ([]stack.BuildOption, error) {

	var buildOptions = []stack.BuildOption{}

	pathToTemplateYAML := fmt.Sprintf("./template/%s/template.yml", language)

	if _, err := os.Stat(pathToTemplateYAML); os.IsNotExist(err) {
		return buildOptions, err
	}

	var langTemplate stack.LanguageTemplate
	parsedLangTemplate, err := stack.ParseYAMLForLanguageTemplate(pathToTemplateYAML)

	if err != nil {
		return buildOptions, err
	}

	if parsedLangTemplate != nil {
		langTemplate = *parsedLangTemplate
		buildOptions = langTemplate.BuildOptions
	}

	return buildOptions, nil
}

func getPackages(availableBuildOptions []stack.BuildOption, requestedBuildOptions []string) ([]string, bool) {
	var buildPackages []string

	for _, requestedOption := range requestedBuildOptions {

		requestedOptionAvailable := false

		for _, availableOption := range availableBuildOptions {

			if availableOption.Name == requestedOption {
				buildPackages = append(buildPackages, availableOption.Packages...)
				requestedOptionAvailable = true
				break
			}
		}
		if requestedOptionAvailable == false {
			return buildPackages, false
		}
	}

	return deDuplicate(buildPackages), true
}

func deDuplicate(buildOptPackages []string) []string {

	seenPackages := map[string]bool{}
	retPackages := []string{}

	for _, packageName := range buildOptPackages {

		if _, alreadySeen := seenPackages[packageName]; !alreadySeen {

			seenPackages[packageName] = true
			retPackages = append(retPackages, packageName)
		}
	}
	return retPackages
}

func isLanguageTemplate(language string) bool {
	return strings.ToLower(language) != "dockerfile"
}
