// Copyright © 2022 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package parser

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	parse2 "github.com/containers/buildah/pkg/parse"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/sealerio/sealer/build/kubefile/command"
	"github.com/sealerio/sealer/pkg/define/application/version"
	v12 "github.com/sealerio/sealer/pkg/define/image/v1"
	"github.com/sealerio/sealer/pkg/define/options"
	"github.com/sealerio/sealer/pkg/imageengine"
)

// LegacyContext stores legacy information during the process of parsing.
// After the parsing ends, return the context to caller, and let the caller
// decide to clean.
type LegacyContext struct {
	files       []string
	directories []string
	// this is a map for appname to related-files
	// only used in test.
	apps2Files map[string][]string
}

type KubefileResult struct {
	Dockerfile         string
	RawCmds            []string
	AppNames           []string
	Applications       map[string]version.VersionedApplication
	ApplicationConfigs []*v12.ApplicationConfig `json:"applicationConfigs"`
	legacyContext      LegacyContext
}

type KubefileParser struct {
	appRootPathFunc func(name string) string
	// path to build context
	buildContext string
	platform     string
	pullPolicy   string
	imageEngine  imageengine.Interface
}

func (kp *KubefileParser) ParseKubefile(rwc io.Reader) (*KubefileResult, error) {
	result, err := parse(rwc)
	if err != nil {
		return nil, fmt.Errorf("failed to parse dockerfile: %v", err)
	}

	mainNode := result.AST
	return kp.generateResult(mainNode)
}

func (kp *KubefileParser) generateResult(mainNode *Node) (*KubefileResult, error) {
	var (
		result = &KubefileResult{
			Applications: map[string]version.VersionedApplication{},
			legacyContext: LegacyContext{
				files:       []string{},
				directories: []string{},
				apps2Files:  map[string][]string{},
			},
			RawCmds:  []string{},
			AppNames: []string{},
		}

		err error

		launchCnt = 0
		cmdsCnt   = 0
		cmdCnt    = 0
	)

	defer func() {
		if err != nil {
			if err2 := result.CleanLegacyContext(); err2 != nil {
				logrus.Warn(err2)
			}
		}
	}()

	// pre-action for commands
	// for FROM, it will try to pull the image, and get apps from "FROM" image
	// for LAUNCH, it will check if it's the last line
	for i, node := range mainNode.Children {
		_command := node.Value
		if _, ok := command.SupportedCommands[_command]; !ok {
			return nil, errors.Errorf("command %s is not supported", _command)
		}

		switch _command {
		case command.From:
			// process FROM aims to pull the image, and merge the applications from
			// the FROM image.
			if err = kp.processFrom(node, result); err != nil {
				return nil, fmt.Errorf("failed to process from: %v", err)
			}
		case command.Launch:
			launchCnt++
			if launchCnt > 1 {
				return nil, errors.New("only one launch could be specified")
			}
			if i != len(mainNode.Children)-1 {
				return nil, errors.New("launch should be the last instruction")
			}

		case command.Cmds:
			cmdsCnt++
			if cmdsCnt > 1 {
				return nil, errors.New("only one cmds could be specified")
			}
			if i != len(mainNode.Children)-1 {
				return nil, errors.New("cmds should be the last instruction")
			}

		case command.Cmd:
			cmdCnt++
			if cmdCnt > 1 {
				break
			}
			logrus.Warn("CMD is about to be deprecated.")
		}

		if cmdCnt >= 1 && launchCnt == 1 {
			return nil, errors.New("cmd and launch are mutually exclusive")
		}

		if err = kp.processOnCmd(result, node); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (kp *KubefileParser) processOnCmd(result *KubefileResult, node *Node) error {
	cmd := node.Value
	switch cmd {
	case command.Label, command.Maintainer, command.Add, command.Arg, command.From, command.Run:
		result.Dockerfile = mergeLines(result.Dockerfile, node.Original)
		return nil
	case command.App:
		_, err := kp.processApp(node, result)
		return err
	case command.AppCmds:
		return kp.processAppCmds(node, result)
	case command.CNI:
		return kp.processCNI(node, result)
	case command.CSI:
		return kp.processCSI(node, result)
	case command.KUBEVERSION:
		return kp.processKubeVersion(node, result)
	case command.Launch:
		return kp.processLaunch(node, result)
	case command.Cmds:
		return kp.processCmds(node, result)
	case command.Copy:
		return kp.processCopy(node, result)
	case command.Cmd:
		return kp.processCmd(node, result)
	default:
		return fmt.Errorf("failed to recognize cmd: %s", cmd)
	}
}

func (kp *KubefileParser) processCNI(node *Node, result *KubefileResult) error {
	app, err := kp.processApp(node, result)
	if err != nil {
		return err
	}
	dockerFileInstruction := fmt.Sprintf(`LABEL %s%s="true"`, command.LabelKubeCNIPrefix, app.Name())
	result.Dockerfile = mergeLines(result.Dockerfile, dockerFileInstruction)
	return nil
}

func (kp *KubefileParser) processCSI(node *Node, result *KubefileResult) error {
	app, err := kp.processApp(node, result)
	if err != nil {
		return err
	}
	dockerFileInstruction := fmt.Sprintf(`LABEL %s%s="true"`, command.LabelKubeCSIPrefix, app.Name())
	result.Dockerfile = mergeLines(result.Dockerfile, dockerFileInstruction)
	return nil
}

func (kp *KubefileParser) processKubeVersion(node *Node, result *KubefileResult) error {
	kubeVersionValue := node.Next.Value
	dockerFileInstruction := fmt.Sprintf(`LABEL %s=%s`, command.LabelSupportedKubeVersionAlpha, strconv.Quote(kubeVersionValue))
	result.Dockerfile = mergeLines(result.Dockerfile, dockerFileInstruction)
	return nil
}

func (kp *KubefileParser) processCopy(node *Node, result *KubefileResult) error {
	if node.Next == nil || node.Next.Next == nil {
		return fmt.Errorf("line %d: invalid copy instruction: %s", node.StartLine, node.Original)
	}

	copySrc := node.Next.Value
	copyDest := node.Next.Next.Value
	// support ${arch} on Kubefile COPY instruction
	// For example:
	// if arch is amd64
	// `COPY ${ARCH}/* .` will be mutated to `COPY amd64/* .`
	// `COPY $ARCH/* .` will be mutated to `COPY amd64/* .`
	_, arch, _, err := parse2.Platform(kp.platform)
	if err != nil {
		return fmt.Errorf("failed to parse platform: %v", err)
	}

	ex := shell.NewLex('\\')
	src, err := ex.ProcessWordWithMap(copySrc, map[string]string{"ARCH": arch})
	if err != nil {
		return fmt.Errorf("failed to render COPY instruction: %v", err)
	}

	tmpLine := strings.Join(append([]string{command.Copy}, src, copyDest), " ")
	result.Dockerfile = mergeLines(result.Dockerfile, tmpLine)

	return nil
}

func (kp *KubefileParser) processAppCmds(node *Node, result *KubefileResult) error {
	appNode := node.Next
	appName := appNode.Value
	if appName == "" {
		return errors.New("app name should be specified in the APPCMD instruction")
	}

	// check whether the app name exist
	var appExisted bool
	for existAppName := range result.Applications {
		if existAppName == appName {
			appExisted = true
		}
	}
	if !appExisted {
		return fmt.Errorf("the specified app name(%s) for `APPCMDS` should be exist", appName)
	}

	// get app cmds value
	tmpPrefix := fmt.Sprintf("%s %s", strings.TrimSpace(strings.ToUpper(command.AppCmds)), strings.TrimSpace(appName))
	appCmdsStr := strings.TrimSpace(strings.TrimPrefix(node.Original, tmpPrefix))
	var appCmds []string
	if err := json.Unmarshal([]byte(appCmdsStr), &appCmds); err != nil {
		return errors.Wrapf(err, `the APPCMDS value should be format: APPCMDS appName ["executable","param1","param2","..."]`)
	}

	var existedApplicationConfig bool
	for _, appConfig := range result.ApplicationConfigs {
		if appConfig.Name != appName {
			continue
		}
		existedApplicationConfig = true
		if appConfig.Launch == nil {
			appConfig.Launch = &v12.ApplicationConfigLaunch{}
		}
		appConfig.Launch.CMDs = appCmds
	}
	if !existedApplicationConfig {
		result.ApplicationConfigs = append(result.ApplicationConfigs, &v12.ApplicationConfig{
			Name: appName,
			Launch: &v12.ApplicationConfigLaunch{
				CMDs: appCmds,
			},
		})
	}
	return nil
}

func (kp *KubefileParser) processCmd(node *Node, result *KubefileResult) error {
	original := node.Original
	cmd := strings.Split(original, "CMD ")
	node.Next.Value = cmd[1]
	result.RawCmds = append(result.RawCmds, node.Next.Value)
	return nil
}

func (kp *KubefileParser) processCmds(node *Node, result *KubefileResult) error {
	cmdsNode := node.Next
	for iter := cmdsNode; iter != nil; iter = iter.Next {
		result.RawCmds = append(result.RawCmds, iter.Value)
	}
	return nil
}

func (kp *KubefileParser) processLaunch(node *Node, result *KubefileResult) error {
	appNode := node.Next
	for iter := appNode; iter != nil; iter = iter.Next {
		appName := iter.Value
		appName = strings.TrimSpace(appName)
		if _, ok := result.Applications[appName]; !ok {
			return errors.Errorf("application %s does not exist in the image", appName)
		}
		result.AppNames = append(result.AppNames, appName)
	}

	return nil
}

func (kp *KubefileParser) processFrom(node *Node, result *KubefileResult) error {
	var (
		platform  = parse2.DefaultPlatform()
		flags     = node.Flags
		imageNode = node.Next
	)
	if len(flags) > 0 {
		f, err := parseListFlag(flags[0])
		if err != nil {
			return err
		}
		if f.flag != "platform" {
			return errors.Errorf("flag %s is not available in FROM", f.flag)
		}
		platform = f.items[0]
	}

	if imageNode == nil || len(imageNode.Value) == 0 {
		return errors.Errorf("image should be specified in the FROM")
	}
	image := imageNode.Value
	if image == "scratch" {
		return nil
	}

	if err := kp.imageEngine.Pull(&options.PullOptions{
		PullPolicy: kp.pullPolicy,
		Image:      image,
		Platform:   platform,
	}); err != nil {
		return fmt.Errorf("failed to pull image %s: %v", image, err)
	}

	extension, err := kp.imageEngine.GetSealerImageExtension(&options.GetImageAnnoOptions{ImageNameOrID: image})
	if err != nil {
		return fmt.Errorf("failed to get image-extension %s: %s", image, err)
	}

	for _, app := range extension.Applications {
		// for range has problem.
		// can't assign address to the target.
		// we should use temp value.
		// https://github.com/golang/gofrontend/blob/e387439bfd24d5e142874b8e68e7039f74c744d7/go/statements.cc#L5501
		theApp := app
		result.Applications[app.Name()] = theApp
	}

	return nil
}

func (kr *KubefileResult) CleanLegacyContext() error {
	var (
		lc  = kr.legacyContext
		err error
	)

	for _, f := range lc.files {
		err = os.Remove(f)
	}

	for _, dir := range lc.directories {
		err = os.RemoveAll(dir)
	}

	return errors.Wrap(err, "failed to clean legacy context")
}

func NewParser(appRootPath string,
	buildOptions options.BuildOptions,
	imageEngine imageengine.Interface,
	platform string) *KubefileParser {
	return &KubefileParser{
		// application will be put under approot/name/
		appRootPathFunc: func(name string) string {
			return makeItDir(filepath.Join(appRootPath, name))
		},
		imageEngine:  imageEngine,
		buildContext: buildOptions.ContextDir,
		pullPolicy:   buildOptions.PullPolicy,
		platform:     platform,
	}
}
