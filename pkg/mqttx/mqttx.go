package mqttx

import (
	"fmt"
	"log/slog"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Config struct {
	BrokerURL string
	ClientID  string
	Username  string
	Password  string
}

func New(cfg Config) (mqtt.Client, error) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.BrokerURL)
	opts.SetClientID(cfg.ClientID)
	opts.SetUsername(cfg.Username)
	opts.SetPassword(cfg.Password)
	opts.SetKeepAlive(120 * time.Second)
	opts.SetPingTimeout(30 * time.Second)
	opts.SetCleanSession(false)
	opts.SetResumeSubs(true)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(2 * time.Second)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		slog.Info("mqtt connected", "broker", cfg.BrokerURL, "client_id", cfg.ClientID)
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		slog.Error("mqtt connection lost", "err", err)
	})

	cli := mqtt.NewClient(opts)
	tok := cli.Connect()
	if !tok.WaitTimeout(20 * time.Second) {
		return nil, fmt.Errorf("mqtt connect timeout")
	}
	if err := tok.Error(); err != nil {
		return nil, err
	}
	return cli, nil
}

func SharedSubscription(group, topic string) string {
	if group == "" {
		return topic
	}
	return "$share/" + group + "/" + topic
}
