package integration

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func mqttBroker(t *testing.T) string {
	t.Helper()
	v := os.Getenv("MQTT_TEST_BROKER")
	if v == "" {
		t.Skip("MQTT_TEST_BROKER not set; skipping integration test")
	}
	return v
}

func newClient(t *testing.T, id string, broker string) mqtt.Client {
	t.Helper()
	opts := mqtt.NewClientOptions().AddBroker(broker).SetClientID(id)
	c := mqtt.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("connect failed: %v", tok.Error())
	}
	return c
}

func TestBroadcastDeliveryToAllSubscribers(t *testing.T) {
	broker := mqttBroker(t)
	topic := fmt.Sprintf("rwct/test/broadcast/%d", time.Now().UnixNano())

	pub := newClient(t, fmt.Sprintf("pub-%d", time.Now().UnixNano()), broker)
	defer pub.Disconnect(100)
	s1 := newClient(t, fmt.Sprintf("s1-%d", time.Now().UnixNano()), broker)
	defer s1.Disconnect(100)
	s2 := newClient(t, fmt.Sprintf("s2-%d", time.Now().UnixNano()), broker)
	defer s2.Disconnect(100)

	var c1, c2 atomic.Int64
	t1 := s1.Subscribe(topic, 1, func(_ mqtt.Client, _ mqtt.Message) { c1.Add(1) })
	if !t1.WaitTimeout(5*time.Second) || t1.Error() != nil {
		t.Fatal(t1.Error())
	}
	t2 := s2.Subscribe(topic, 1, func(_ mqtt.Client, _ mqtt.Message) { c2.Add(1) })
	if !t2.WaitTimeout(5*time.Second) || t2.Error() != nil {
		t.Fatal(t2.Error())
	}

	for i := 0; i < 5; i++ {
		pub.Publish(topic, 1, false, []byte("x"))
	}
	time.Sleep(1200 * time.Millisecond)

	if c1.Load() != 5 || c2.Load() != 5 {
		t.Fatalf("expected both subscribers to receive all messages, got c1=%d c2=%d", c1.Load(), c2.Load())
	}
}

func TestSharedSubscriptionSingleConsumerDelivery(t *testing.T) {
	broker := mqttBroker(t)
	topic := fmt.Sprintf("rwct/test/queue/%d", time.Now().UnixNano())
	shared := "$share/analyzers/" + topic

	pub := newClient(t, fmt.Sprintf("pub2-%d", time.Now().UnixNano()), broker)
	defer pub.Disconnect(100)
	s1 := newClient(t, fmt.Sprintf("qs1-%d", time.Now().UnixNano()), broker)
	defer s1.Disconnect(100)
	s2 := newClient(t, fmt.Sprintf("qs2-%d", time.Now().UnixNano()), broker)
	defer s2.Disconnect(100)

	var c1, c2 atomic.Int64
	t1 := s1.Subscribe(shared, 1, func(_ mqtt.Client, _ mqtt.Message) { c1.Add(1) })
	if !t1.WaitTimeout(5*time.Second) || t1.Error() != nil {
		t.Fatal(t1.Error())
	}
	t2 := s2.Subscribe(shared, 1, func(_ mqtt.Client, _ mqtt.Message) { c2.Add(1) })
	if !t2.WaitTimeout(5*time.Second) || t2.Error() != nil {
		t.Fatal(t2.Error())
	}

	for i := 0; i < 10; i++ {
		pub.Publish(topic, 1, false, []byte("x"))
	}
	time.Sleep(1500 * time.Millisecond)

	total := c1.Load() + c2.Load()
	if total != 10 {
		t.Fatalf("expected shared subscribers to consume exactly 10 total, got c1=%d c2=%d", c1.Load(), c2.Load())
	}
}
