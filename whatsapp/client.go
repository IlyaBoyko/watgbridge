package whatsapp

import (
	"context"
	"fmt"
	"time"

	"watgbridge/retry"
	"watgbridge/state"

	_ "github.com/jackc/pgx/v5"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	waWa6 "go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type whatsmeowLogger struct {
	logger *zap.SugaredLogger
}

func (wl whatsmeowLogger) Warnf(msg string, args ...interface{}) {
	wl.logger.Warnf(msg, args...)
	_ = wl.logger.Sync()
}
func (wl whatsmeowLogger) Errorf(msg string, args ...interface{}) {
	wl.logger.Errorf(msg, args...)
	_ = wl.logger.Sync()
}
func (wl whatsmeowLogger) Infof(msg string, args ...interface{}) {
	wl.logger.Infof(msg, args...)
	_ = wl.logger.Sync()
}
func (wl whatsmeowLogger) Debugf(msg string, args ...interface{}) {
	wl.logger.Debugf(msg, args...)
	_ = wl.logger.Sync()
}
func (wl whatsmeowLogger) Sub(module string) waLog.Logger {
	return whatsmeowLogger{logger: wl.logger.Named(module)}
}

// WhatsApp client identities the session can register as.
const (
	clientModeAndroid         = "android"
	clientModeAndroidBusiness = "android_business"
)

// connectPolicy paces reconnects of a linked session: 30s, 1m, 2m ... 30m.
var connectPolicy = retry.Policy{Initial: 30 * time.Second, Max: 30 * time.Minute}

func NewWhatsAppClient() error {

	var (
		cfg    = state.State.Config
		err    error
		logger *zap.Logger
	)

	if cfg.WhatsApp.WhatsmeowDebugMode {
		developmentConfig := zap.NewDevelopmentConfig()
		developmentConfig.OutputPaths = append(developmentConfig.OutputPaths, "whatsmeow_debug.log")
		logger, err = developmentConfig.Build()
		if err != nil {
			panic(fmt.Errorf("failed to initialize development loggers for WhatsMeow client: %s", err))
		}
	} else {
		productionConfig := zap.NewProductionConfig()
		logger, err = productionConfig.Build()
		if err != nil {
			panic(fmt.Errorf("failed to initialize production loggers for WhatsMeow client: %s", err))
		}
	}
	logger = logger.Named("WaTgBridge")
	defer logger.Sync()

	waDatabaseLogger := &whatsmeowLogger{logger: logger.Sugar().Named("WhatsMeow_Database")}
	waClientLogger := &whatsmeowLogger{logger: logger.Sugar().Named("WhatsMeow_Client")}

	// Configure device as Android if client mode is set to android/android_business
	isAndroidEmulation := cfg.WhatsApp.ClientMode == clientModeAndroid ||
		cfg.WhatsApp.ClientMode == clientModeAndroidBusiness

	if isAndroidEmulation {
		store.DeviceProps.Os = proto.String("Android")
		store.DeviceProps.RequireFullSync = proto.Bool(false)
		store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_ANDROID_PHONE.Enum()
		store.DeviceProps.Version = &waCompanionReg.DeviceProps_AppVersion{
			Primary:    proto.Uint32(2),
			Secondary:  proto.Uint32(23),
			Tertiary:   proto.Uint32(9),
			Quaternary: proto.Uint32(0),
		}
		store.DeviceProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:              proto.Uint32(0),
			FullSyncSizeMbLimit:            proto.Uint32(0),
			StorageQuotaMb:                 proto.Uint32(0),
			RecentSyncDaysLimit:            proto.Uint32(0),
			SupportCallLogHistory:          proto.Bool(false),
			SupportBotUserAgentChatHistory: proto.Bool(false),
			SupportCagReactionsAndPolls:    proto.Bool(false),
		}

		// Configure client payload as Android
		var platform waWa6.ClientPayload_UserAgent_Platform
		if cfg.WhatsApp.ClientMode == clientModeAndroidBusiness {
			platform = waWa6.ClientPayload_UserAgent_SMB_ANDROID
		} else {
			platform = waWa6.ClientPayload_UserAgent_ANDROID
		}
		store.BaseClientPayload.UserAgent.Platform = platform.Enum()
		store.BaseClientPayload.UserAgent.Device = proto.String("SM-G900F")
		store.BaseClientPayload.UserAgent.Manufacturer = proto.String("Samsung")
		store.BaseClientPayload.UserAgent.OsVersion = proto.String("14")
		waVersion := store.GetWAVersion()
		store.BaseClientPayload.UserAgent.AppVersion = &waWa6.ClientPayload_UserAgent_AppVersion{
			Primary:   proto.Uint32(uint32(waVersion[0])),
			Secondary: proto.Uint32(uint32(waVersion[1])),
			Tertiary:  proto.Uint32(uint32(waVersion[2])),
		}
		store.BaseClientPayload.WebInfo = nil // Remove WebInfo as it's only for web

		if cfg.WhatsApp.ClientMode == clientModeAndroidBusiness {
			logger.Info("WhatsApp client configured to emulate Android phone (WhatsApp Business)")
		} else {
			logger.Info("WhatsApp client configured to emulate Android phone")
		}
	} else {
		// Keep default Web configuration
		store.DeviceProps.Os = proto.String(state.State.Config.WhatsApp.SessionName)
		store.DeviceProps.RequireFullSync = proto.Bool(false)
		store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
		store.DeviceProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:              proto.Uint32(0),
			FullSyncSizeMbLimit:            proto.Uint32(0),
			StorageQuotaMb:                 proto.Uint32(0),
			RecentSyncDaysLimit:            proto.Uint32(0),
			SupportCallLogHistory:          proto.Bool(false),
			SupportBotUserAgentChatHistory: proto.Bool(false),
			SupportCagReactionsAndPolls:    proto.Bool(false),
		}

		logger.Info("WhatsApp client configured as Web client")
	}

	container, err := sqlstore.New(context.Background(), state.State.Config.WhatsApp.LoginDatabase.Type,
		state.State.Config.WhatsApp.LoginDatabase.URL, waDatabaseLogger)
	if err != nil {
		return fmt.Errorf("could not initialize sqlstore for Whatsapp : %s", err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return fmt.Errorf("could not initialize device store for Whatsapp : %s", err)
	}

	client := whatsmeow.NewClient(deviceStore, waClientLogger)
	state.State.WhatsAppClient = client

	// Pairing only ever starts on the owner's command (WaitForPairing), so
	// a restart can never become a loop of pairing connections.
	if client.Store.ID == nil {
		return ErrNotPaired
	}

	// A failed connect is retried here with long pauses rather than
	// returned: a crash would restart the container and reconnect at once.
	err = retry.Do(context.Background(), connectPolicy, nil, client.Connect,
		func(attempt int, wait time.Duration, err error) {
			logger.Warn("could not connect to WhatsApp, retrying",
				zap.Int("attempt", attempt), zap.Duration("retry_in", wait), zap.Error(err))
		})
	if err != nil {
		return fmt.Errorf("could not connect to Whatsapp : %s", err)
	}

	logger.Info("successfully logged into WhatsApp",
		zap.String("push_name", client.Store.PushName),
		zap.String("jid", client.Store.ID.String()),
	)

	return nil
}
