package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dropbox/godropbox/errors"
	"github.com/gin-gonic/gin"
	"github.com/pritunl/pritunl-client-electron/service/auth"
	"github.com/pritunl/pritunl-client-electron/service/autoclean"
	"github.com/pritunl/pritunl-client-electron/service/config"
	"github.com/pritunl/pritunl-client-electron/service/constants"
	"github.com/pritunl/pritunl-client-electron/service/errortypes"
	"github.com/pritunl/pritunl-client-electron/service/handlers"
	"github.com/pritunl/pritunl-client-electron/service/logger"
	"github.com/pritunl/pritunl-client-electron/service/profile"
	"github.com/pritunl/pritunl-client-electron/service/setup"
	"github.com/pritunl/pritunl-client-electron/service/tuntap"
	"github.com/pritunl/pritunl-client-electron/service/update"
	"github.com/pritunl/pritunl-client-electron/service/utils"
	"github.com/pritunl/pritunl-client-electron/service/watch"
	"github.com/pritunl/pritunl-client-electron/service/winsvc"
	"github.com/sirupsen/logrus"
)

func main() {
	install := flag.Bool("install", false, "run post install")
	uninstall := flag.Bool("uninstall", false, "run pre uninstall")
	devPtr := flag.Bool("dev", false, "development mode")
	flag.Parse()

	if *install {
		setup.Install()
		return
	}

	if *uninstall {
		setup.Uninstall()
		return
	}

	if *devPtr {
		constants.Development = true
	}

	err := config.Load()
	if err != nil {
		panic(err)
	}

	err = utils.PidInit()
	if err != nil {
		panic(err)
	}

	err = utils.InitTempDir()
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("main: Failed to init temp dir")
		panic(err)
	}

	if runtime.GOOS == "darwin" {
		output, err := utils.ExecOutput("uname", "-r")
		if err == nil {
			macosVersion, err := strconv.Atoi(strings.Split(output, ".")[0])
			if err == nil && macosVersion < 20 {
				constants.Macos10 = true
			}
		}
	}

	logger.Init()

	logrus.WithFields(logrus.Fields{
		"version": constants.Version,
	}).Info("main: Service starting")

	go update.Check()

	defer func() {
		panc := recover()
		if panc != nil {
			logrus.WithFields(logrus.Fields{
				"stack": string(debug.Stack()),
				"panic": panc,
			}).Error("main: Panic")
			panic(panc)
		}
	}()

	err = auth.Init()
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("main: Failed to init auth")
		panic(err)
	}

	err = autoclean.CheckAndClean()
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("main: Failed to run check and clean")
		panic(err)
	}

	if runtime.GOOS == "windows" {
		if config.Config.DisableNetClean {
			logrus.Info("main: Network clean disabled")
		} else {
			err = tuntap.Clean()
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("main: Failed to clear interfaces")
				err = nil
			}
		}
	}

	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	handlers.Register(router)

	watch.StartWatch()

	server := &http.Server{
		Addr:           "127.0.0.1:9770",
		Handler:        router,
		ReadTimeout:    300 * time.Second,
		WriteTimeout:   300 * time.Second,
		MaxHeaderBytes: 4096,
	}

	err = profile.Clean()
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("main: Failed to clean profiles")
		panic(err)
	}

	go func() {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			err = server.ListenAndServe()
			if err != nil {
				err = &errortypes.WriteError{
					errors.Wrap(err, "main: Server listen error"),
				}
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("main: Server error")
			}
		} else {
			_ = os.Remove("/var/run/pritunl.sock")

			listener, err := net.Listen("unix", "/var/run/pritunl.sock")
			if err != nil {
				err = &errortypes.WriteError{
					errors.Wrap(err, "main: Failed to create unix socket"),
				}
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("main: Server error")
			}

			err = os.Chmod("/var/run/pritunl.sock", 0777)
			if err != nil {
				err = &errortypes.WriteError{
					errors.Wrap(err, "main: Failed to chmod unix socket"),
				}
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("main: Server error")
			}

			err = server.Serve(listener)
			if err != nil {
				err = &errortypes.WriteError{
					errors.Wrap(err, "main: Server listen error"),
				}
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("main: Server error")
			}
		}
	}()

	profile.WatchSystemProfiles()

	if winsvc.IsWindowsService() {
		service := winsvc.New()

		err = service.Run()
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Error("main: Service error")
			panic(err)
		}
	} else {
		sig := make(chan os.Signal, 100)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
	}

	webCtx, webCancel := context.WithTimeout(
		context.Background(),
		1*time.Second,
	)
	defer webCancel()

	func() {
		defer func() {
			recover()
		}()
		server.Shutdown(webCtx)
		server.Close()
	}()

	time.Sleep(250 * time.Millisecond)

	profile.Shutdown()

	prfls := profile.GetProfiles()
	for _, prfl := range prfls {
		prfl.StopBackground()
	}

	for _, prfl := range prfls {
		prfl.Wait()
	}

	if runtime.GOOS == "darwin" {
		_ = utils.ClearScutilConnKeys()
		_ = utils.RestoreScutilDns(true)
	}

	time.Sleep(750 * time.Millisecond)
}
