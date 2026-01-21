package pkg

import (
	"log"
	"time"

	ytsdk "go.ytsaurus.tech/yt/go/yt"
	ythttpsdk "go.ytsaurus.tech/yt/go/yt/ythttp"
)

type SimpleLogger struct{}

func (SimpleLogger) Debugf(format string, args ...any) {
	log.Printf("DEBUG: "+format, args...)
}
func (SimpleLogger) Infof(format string, args ...any) {
	log.Printf("INFO:  "+format, args...)
}
func (SimpleLogger) Warnf(format string, args ...any) {
	log.Printf("WARN:  "+format, args...)
}
func (SimpleLogger) Errorf(format string, args ...any) {
	log.Printf("ERROR: "+format, args...)
}

func CreateYTClient(proxy string, credentials ytsdk.Credentials) (ytsdk.Client, error) {
	timeout := time.Second * 10
	return ythttpsdk.NewClient(&ytsdk.Config{
		Proxy:                 proxy,
		Credentials:           credentials,
		LightRequestTimeout:   &timeout,
		DisableProxyDiscovery: true,
	})
}
