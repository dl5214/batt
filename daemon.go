package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	ginlogrus "github.com/toorop/gin-logrus"

	"github.com/charlie0129/batt/smc"
)

var (
	smcConn        *smc.Connection
	unixSocketPath = "/var/run/batt.sock"
)

func setupRoutes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	router := gin.Default()
	router.Use(ginlogrus.Logger(logrus.StandardLogger()), gin.Recovery())
	router.GET("/config", getConfig)
	router.PUT("/config", setConfig) // Should not be called by user.
	router.GET("/limit", getLimit)
	router.PUT("/limit", setLimit)
	router.PUT("/prevent-idle-sleep", setPreventIdleSleep)
	router.PUT("/disable-charging-pre-sleep", setDisableChargingPreSleep)

	return router
}

func runDaemon() {
	router := setupRoutes()

	err := loadConfig()
	if err != nil {
		logrus.Fatal(err)
	}
	logrus.Infof("config loaded: %#v", config)

	srv := &http.Server{
		Handler: router,
	}

	// Create the socket to listen on:
	l, err := net.Listen("unix", unixSocketPath)
	if err != nil {
		logrus.Fatal(err)
		return
	}

	// Serve HTTP on unix socket
	go func() {
		logrus.Infof("http server listening on %s", l.Addr().String())
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logrus.Fatal(err)
		}
	}()

	// Listen to system sleep notifications.
	go func() {
		err := listenNotifications()
		if err != nil {
			logrus.Errorf("failed to listen to system sleep notifications: %v", err)
			os.Exit(1)
		}
	}()

	// Open Apple SMC for read/writing
	smcConn = smc.New()
	if err := smcConn.Open(); err != nil {
		logrus.Fatal(err)
	}

	go func() {
		logrus.Infof("main loop starts")

		//nolint:revive // not empty
		for mainLoop() {
		}

		logrus.Errorf("main loop exited unexpectedly")
	}()

	// Handle common process-killing signals so we can gracefully shut down:
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)

	// Wait for a SIGINT or SIGKILL:
	sig := <-sigc
	logrus.Infof("Caught signal %s: shutting down.", sig)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = srv.Shutdown(ctx)
	if err != nil {
		logrus.Errorf("failed to shutdown http server: %v", err)
	}
	cancel()
	// Stop listening (and unlink the socket if unix type):
	err = l.Close()
	if err != nil {
		logrus.Errorf("failed to close socket: %v", err)
	}
	err = smcConn.Close()
	if err != nil {
		logrus.Errorf("failed to close smc connection: %v", err)
	}
	err = saveConfig()
	if err != nil {
		logrus.Errorf("failed to save config: %v", err)
	}
	os.Exit(0)
}