package main

import (
	"os"
	"os/signal"
	"syscall"

	log "github.com/sirupsen/logrus"
)

func onSigUSR1(c chan os.Signal) {
	for {
		<-c
		log.SetLevel(log.DebugLevel)
		log.Info("Switching log level to DEBUG")
	}
}

func onSigUSR2(c chan os.Signal) {
	for {
		<-c
		log.SetLevel(log.InfoLevel)
		log.Info("Switching log level to INFO")
	}
}

func ControlLogLevelViaSignals() {
	usr1Channel := make(chan os.Signal, 10)
	signal.Notify(usr1Channel, syscall.SIGUSR1)
	go onSigUSR1(usr1Channel)

	usr2Channel := make(chan os.Signal, 10)
	signal.Notify(usr2Channel, syscall.SIGUSR2)
	go onSigUSR2(usr2Channel)
}
