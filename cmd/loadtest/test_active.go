// Copyright (c) 2016 Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mattermost/mattermost-load-test/cmd/cmdlib"
	"github.com/mattermost/mattermost-load-test/loadtestconfig"
	"github.com/mattermost/platform/model"
	"github.com/spf13/cobra"

	"github.com/paulbellamy/ratecounter"
)

type UserEntityPosterConfiguration struct {
	PostingFrequencySeconds int
}

func NewUserEntityPosterConfig(config *UserEntityConfig) UserEntityPosterConfiguration {
	var userEntityPosterConfig UserEntityPosterConfiguration
	loadtestconfig.UnmarshalConfigStruct(&userEntityPosterConfig)

	if userEntityPosterConfig.PostingFrequencySeconds == 0 {
		userEntityPosterConfig.PostingFrequencySeconds = 1
	}

	return userEntityPosterConfig
}

func testActiveCmd(cmd *cobra.Command, args []string) {
	context := cmdlib.MakeCommandContext()

	testActive(context)
}

func doPrintStats(c *cmdlib.CommandContext, actionsCounter *ratecounter.RateCounter, errorCounter *ratecounter.RateCounter, stopChan <-chan bool) {
	// Print statistics on timer
	statsTicker := time.NewTicker(time.Second * 3)
	defer statsTicker.Stop()

	for {
		select {
		case <-stopChan:
			return
		case <-statsTicker.C:
			c.Println("Actions per second: " + strconv.Itoa(int(actionsCounter.Rate())))
			c.Println("Errors per second: " + strconv.Itoa(int(errorCounter.Rate())))
		}
	}
}

func UserEntityStatusPrinter(c *cmdlib.CommandContext, statusChan <-chan UserEntityStatusReport, stopChan <-chan bool, stopWait *sync.WaitGroup) {
	defer stopWait.Done()
	logfile, err := os.Create("status.log")
	if err != nil {
		c.PrintError("Unable to open log file for entity statuses")
		return
	}
	defer func() {
		logfile.Sync()
		logfile.Close()
	}()

	// Rate counters for collecting statistics
	actionsCounter := ratecounter.NewRateCounter(1 * time.Second)
	errorCounter := ratecounter.NewRateCounter(1 * time.Second)

	go doPrintStats(c, actionsCounter, errorCounter, stopChan)

	handleReport := func(report *UserEntityStatusReport) {
		if report.Status == STATUS_ACTION {
			actionsCounter.Incr(1)
		} else if report.Status == STATUS_ERROR {
			errorCounter.Incr(1)
		}
		logfile.WriteString(fmt.Sprintln(report))
	}

	// This strange thing makes sure that the statusChan is drained before it will listen to the stopChan
	for {
		select {
		case report := <-statusChan:
			handleReport(&report)
		default:
			select {
			case report := <-statusChan:
				handleReport(&report)
			case <-stopChan:
				return
			}
		}
	}
}

func testActive(c *cmdlib.CommandContext) {
	numEntities := c.LoadTestConfig.UserEntitiesConfiguration.NumClientEntities

	inputState := loadtestconfig.ServerStateFromStdin()

	c.PrettyPrintln("Starting active users load test")

	// Create a channel to signal a stop command
	stopChan := make(chan bool)

	// Create a wait group so we can wait for our entites to complete
	var stopWait sync.WaitGroup

	// Create Channel for users to report status
	statusPrinterStopChan := make(chan bool)
	var printerWait sync.WaitGroup
	statusChannel := make(chan UserEntityStatusReport, 1000)
	printerWait.Add(1)
	go UserEntityStatusPrinter(c, statusChannel, statusPrinterStopChan, &printerWait)

	c.Println("Starting ramp-up")
	for entityNum := 0; entityNum < numEntities; entityNum++ {
		userClient := cmdlib.GetUserClient(&c.LoadTestConfig.ConnectionConfiguration, &inputState.Users[entityNum%len(inputState.Users)])
		userWebsocketClient, err := model.NewWebSocketClient(c.LoadTestConfig.ConnectionConfiguration.WebsocketURL, userClient.AuthToken)
		if err != nil {
			c.PrintErrorln("Unable to setup websocket client", err)
			continue
		}

		config := UserEntityConfig{
			Id:                  entityNum,
			Client:              userClient,
			WebSocketClient:     userWebsocketClient,
			LoadTestConfig:      c.LoadTestConfig,
			StatusReportChannel: statusChannel,
			StopEntityChannel:   stopChan,
			StopEntityWaitGroup: &stopWait,
		}
		stopWait.Add(1)
		go startPosterListenerEntity(config, inputState.Channels[entityNum%len(inputState.Channels)])
		time.Sleep(time.Duration(c.LoadTestConfig.UserEntitiesConfiguration.EntityRampupDistanceMilliseconds) * time.Millisecond)
	}
	c.Println("Ramp-up complete")
	interrupChannel := make(chan os.Signal)
	signal.Notify(interrupChannel, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	<-interrupChannel
	c.Println("Shutdown signal recieved.")
	close(stopChan)

	c.Println("Waiting for user entities")
	stopWait.Wait()
	c.Println("Flushing status reporting channel")
	close(statusPrinterStopChan)
	printerWait.Wait()
	c.Println("DONE!")
}

func startPosterListenerEntity(config UserEntityConfig, channel loadtestconfig.ServerStateChannel) {
	config.StopEntityWaitGroup.Add(1)
	go startWebsocketListenerUserEntity(config)
	startPosterUserEntity(config, channel)
}

func startWebsocketListenerUserEntity(config UserEntityConfig) {
	config.SendStatusLaunching()
	defer config.StopEntityWaitGroup.Done()

	config.WebSocketClient.Listen()

	websocketRetryCount := 0

	config.SendStatusActive("Listening")
	for {
		select {
		case <-config.StopEntityChannel:
			config.SendStatusStopped("")
			return
		case event, ok := <-config.WebSocketClient.EventChannel:
			if !ok {
				if config.WebSocketClient.ListenError != nil {
					config.SendStatusError(config.WebSocketClient.ListenError, "Websocket error")
				} else {
					config.SendStatusError(nil, "Server closed websocket")
				}

				// If we are set to retry connection, first retry immediately, then backoff until retry max is reached
				if config.LoadTestConfig.ConnectionConfiguration.RetryWebsockets {
					if websocketRetryCount > config.LoadTestConfig.ConnectionConfiguration.MaxRetryWebsocket {
						config.SendStatusFailed(nil, "Websocket disconneced. Max retries reached.")
						return
					}
					time.Sleep(time.Duration(websocketRetryCount) * time.Second)
					config.WebSocketClient.Listen()
					websocketRetryCount++
					continue
				} else {
					config.SendStatusFailed(nil, "Websocket disconneced. No Retry.")
					return
				}
			}
			config.SendStatusAction("Recieved websocket event: " + event.Event)
		}
	}
}

func startPosterUserEntity(config UserEntityConfig, channel loadtestconfig.ServerStateChannel) {
	config.SendStatusLaunching()
	defer config.StopEntityWaitGroup.Done()
	posterConfig := NewUserEntityPosterConfig(&config)

	// Allows us to perform our action every x seconds
	postTicker := time.NewTicker(time.Second * time.Duration(posterConfig.PostingFrequencySeconds))
	defer postTicker.Stop()

	config.SendStatusActive("Posting")
	for {
		select {
		case <-config.StopEntityChannel:
			config.SendStatusStopped("")
			return
		case <-postTicker.C:
			config.Client.SetTeamId(channel.TeamId)
			post := &model.Post{
				ChannelId: channel.Id,
				Message:   "Test message",
			}
			_, err := config.Client.CreatePost(post)
			if err != nil {
				config.SendStatusError(err, "Failed to post message")
			} else {
				config.SendStatusAction("Posted Message")
			}
		}
	}
}