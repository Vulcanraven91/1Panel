package service

import (
	"bytes"
	"fmt"
	"github.com/1Panel-dev/1Panel/backend/app/dto/request"
	"github.com/1Panel-dev/1Panel/backend/app/model"
	"github.com/1Panel-dev/1Panel/backend/buserr"
	"github.com/1Panel-dev/1Panel/backend/constant"
	"github.com/1Panel-dev/1Panel/backend/global"
	"github.com/1Panel-dev/1Panel/backend/utils/docker"
	"github.com/1Panel-dev/1Panel/backend/utils/files"
	"github.com/pkg/errors"
	"github.com/subosito/gotenv"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func handleNode(create request.RuntimeCreate, runtime *model.Runtime, fileOp files.FileOp, appVersionDir string) (err error) {
	runtimeDir := path.Join(constant.RuntimeDir, create.Type)
	if err = fileOp.CopyDir(appVersionDir, runtimeDir); err != nil {
		return
	}
	versionDir := path.Join(runtimeDir, filepath.Base(appVersionDir))
	projectDir := path.Join(runtimeDir, create.Name)
	defer func() {
		if err != nil {
			_ = fileOp.DeleteDir(projectDir)
		}
	}()
	if err = fileOp.Rename(versionDir, projectDir); err != nil {
		return
	}
	composeContent, envContent, _, err := handleParams(create, projectDir)
	if err != nil {
		return
	}
	runtime.DockerCompose = string(composeContent)
	runtime.Env = string(envContent)
	runtime.Status = constant.RuntimeStarting
	runtime.CodeDir = create.CodeDir

	go startRuntime(runtime)
	return
}

func handlePHP(create request.RuntimeCreate, runtime *model.Runtime, fileOp files.FileOp, appVersionDir string) (err error) {
	buildDir := path.Join(appVersionDir, "build")
	if !fileOp.Stat(buildDir) {
		return buserr.New(constant.ErrDirNotFound)
	}
	runtimeDir := path.Join(constant.RuntimeDir, create.Type)
	tempDir := filepath.Join(runtimeDir, fmt.Sprintf("%d", time.Now().UnixNano()))
	if err = fileOp.CopyDir(buildDir, tempDir); err != nil {
		return
	}
	oldDir := path.Join(tempDir, "build")
	projectDir := path.Join(runtimeDir, create.Name)
	defer func() {
		if err != nil {
			_ = fileOp.DeleteDir(projectDir)
		}
	}()
	if oldDir != projectDir {
		if err = fileOp.Rename(oldDir, projectDir); err != nil {
			return
		}
		if err = fileOp.DeleteDir(tempDir); err != nil {
			return
		}
	}
	composeContent, envContent, forms, err := handleParams(create, projectDir)
	if err != nil {
		return
	}
	runtime.DockerCompose = string(composeContent)
	runtime.Env = string(envContent)
	runtime.Params = string(forms)
	runtime.Status = constant.RuntimeBuildIng

	go buildRuntime(runtime, "", false)
	return
}

func startRuntime(runtime *model.Runtime) {
	cmd := exec.Command("docker-compose", "-f", runtime.GetComposePath(), "up", "-d")
	logPath := runtime.GetLogPath()
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		global.LOG.Errorf("Failed to open log file: %v", err)
		return
	}
	multiWriterStdout := io.MultiWriter(os.Stdout, logFile)
	cmd.Stdout = multiWriterStdout
	var stderrBuf bytes.Buffer
	multiWriterStderr := io.MultiWriter(&stderrBuf, logFile, os.Stderr)
	cmd.Stderr = multiWriterStderr

	err = cmd.Run()
	if err != nil {
		runtime.Status = constant.RuntimeError
		runtime.Message = buserr.New(constant.ErrRuntimeStart).Error() + ":" + stderrBuf.String()
	} else {
		runtime.Status = constant.RuntimeRunning
		runtime.Message = ""
	}

	_ = runtimeRepo.Save(runtime)
}

func runComposeCmdWithLog(operate string, composePath string, logPath string) error {
	cmd := exec.Command("docker-compose", "-f", composePath, operate)
	if operate == "up" {
		cmd = exec.Command("docker-compose", "-f", composePath, operate, "-d")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		global.LOG.Errorf("Failed to open log file: %v", err)
		return err
	}
	multiWriterStdout := io.MultiWriter(os.Stdout, logFile)
	cmd.Stdout = multiWriterStdout
	var stderrBuf bytes.Buffer
	multiWriterStderr := io.MultiWriter(&stderrBuf, logFile, os.Stderr)
	cmd.Stderr = multiWriterStderr

	err = cmd.Run()
	if err != nil {
		return errors.New(buserr.New(constant.ErrRuntimeStart).Error() + ":" + stderrBuf.String())
	}
	return nil
}

func reCreateRuntime(runtime *model.Runtime) {
	var err error
	defer func() {
		if err != nil {
			runtime.Status = constant.RuntimeError
			runtime.Message = err.Error()
			_ = runtimeRepo.Save(runtime)
		}
	}()
	if err = runComposeCmdWithLog("down", runtime.GetComposePath(), runtime.GetLogPath()); err != nil {
		return
	}
	if err = runComposeCmdWithLog("up", runtime.GetComposePath(), runtime.GetLogPath()); err != nil {
		return
	}
	runtime.Status = constant.RuntimeRunning
	_ = runtimeRepo.Save(runtime)
}

func buildRuntime(runtime *model.Runtime, oldImageID string, rebuild bool) {
	runtimePath := runtime.GetPath()
	composePath := runtime.GetComposePath()
	logPath := path.Join(runtimePath, "build.log")

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		global.LOG.Errorf("failed to open log file: %v", err)
		return
	}
	defer func() {
		_ = logFile.Close()
	}()

	cmd := exec.Command("docker-compose", "-f", composePath, "build")
	multiWriterStdout := io.MultiWriter(os.Stdout, logFile)
	cmd.Stdout = multiWriterStdout
	var stderrBuf bytes.Buffer
	multiWriterStderr := io.MultiWriter(&stderrBuf, logFile, os.Stderr)
	cmd.Stderr = multiWriterStderr

	err = cmd.Run()
	if err != nil {
		runtime.Status = constant.RuntimeError
		runtime.Message = buserr.New(constant.ErrImageBuildErr).Error() + ":" + stderrBuf.String()
	} else {
		runtime.Status = constant.RuntimeNormal
		runtime.Message = ""
		if oldImageID != "" {
			client, err := docker.NewClient()
			if err == nil {
				newImageID, err := client.GetImageIDByName(runtime.Image)
				if err == nil && newImageID != oldImageID {
					global.LOG.Infof("delete imageID [%s] ", oldImageID)
					if err := client.DeleteImage(oldImageID); err != nil {
						global.LOG.Errorf("delete imageID [%s] error %v", oldImageID, err)
					} else {
						global.LOG.Infof("delete old image success")
					}
				}
			} else {
				global.LOG.Errorf("delete imageID [%s] error %v", oldImageID, err)
			}
		}
		if rebuild && runtime.ID > 0 {
			websites, _ := websiteRepo.GetBy(websiteRepo.WithRuntimeID(runtime.ID))
			if len(websites) > 0 {
				installService := NewIAppInstalledService()
				installMap := make(map[uint]string)
				for _, website := range websites {
					if website.AppInstallID > 0 {
						installMap[website.AppInstallID] = website.PrimaryDomain
					}
				}
				for installID, domain := range installMap {
					go func(installID uint, domain string) {
						global.LOG.Infof("rebuild php runtime [%s] domain [%s]", runtime.Name, domain)
						if err := installService.Operate(request.AppInstalledOperate{
							InstallId: installID,
							Operate:   constant.Rebuild,
						}); err != nil {
							global.LOG.Errorf("rebuild php runtime [%s] domain [%s] error %v", runtime.Name, domain, err)
						}
					}(installID, domain)
				}
			}
		}
	}
	_ = runtimeRepo.Save(runtime)
}

func handleParams(create request.RuntimeCreate, projectDir string) (composeContent []byte, envContent []byte, forms []byte, err error) {
	fileOp := files.NewFileOp()
	composeContent, err = fileOp.GetContent(path.Join(projectDir, "docker-compose.yml"))
	if err != nil {
		return
	}
	env, err := gotenv.Read(path.Join(projectDir, ".env"))
	if err != nil {
		return
	}
	switch create.Type {
	case constant.RuntimePHP:
		create.Params["IMAGE_NAME"] = create.Image
		forms, err = fileOp.GetContent(path.Join(projectDir, "config.json"))
		if err != nil {
			return
		}
		if extends, ok := create.Params["PHP_EXTENSIONS"]; ok {
			if extendsArray, ok := extends.([]interface{}); ok {
				strArray := make([]string, len(extendsArray))
				for i, v := range extendsArray {
					strArray[i] = strings.ToLower(fmt.Sprintf("%v", v))
				}
				create.Params["PHP_EXTENSIONS"] = strings.Join(strArray, ",")
			}
		}
		create.Params["CONTAINER_PACKAGE_URL"] = create.Source
	case constant.RuntimeNode:
		create.Params["CODE_DIR"] = create.CodeDir
		create.Params["NODE_VERSION"] = create.Version
		if create.NodeConfig.Install {
			create.Params["RUN_INSTALL"] = "1"
		} else {
			create.Params["RUN_INSTALL"] = "0"
		}
	}

	newMap := make(map[string]string)
	handleMap(create.Params, newMap)
	for k, v := range newMap {
		env[k] = v
	}
	envStr, err := gotenv.Marshal(env)
	if err != nil {
		return
	}
	if err = gotenv.Write(env, path.Join(projectDir, ".env")); err != nil {
		return
	}
	envContent = []byte(envStr)
	return
}
