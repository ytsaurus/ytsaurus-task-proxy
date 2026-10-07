package pkg

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultLogDirectory = "/var/log/task-proxy"

type LoggingConfig struct {
	Directory string          `yaml:"directory"`
	Server    LogConfig       `yaml:"server"`
	Proxy     LogConfig       `yaml:"proxy"`
	AccessLog AccessLogConfig `yaml:"accessLog"`
}

type LogConfig struct {
	WriterType     string          `yaml:"writerType"`
	MinLogLevel    string          `yaml:"minLogLevel"`
	RotationPolicy *RotationPolicy `yaml:"rotationPolicy,omitempty"`
}

type AccessLogConfig struct {
	Enabled        bool            `yaml:"enabled"`
	WriterType     string          `yaml:"writerType"`
	RotationPolicy *RotationPolicy `yaml:"rotationPolicy,omitempty"`
}

type RotationPolicy struct {
	RotationPeriodMilliseconds int64        `yaml:"rotationPeriodMilliseconds,omitempty"`
	MaxSegmentSize             ByteQuantity `yaml:"maxSegmentSize,omitempty"`
	MaxTotalSizeToKeep         ByteQuantity `yaml:"maxTotalSizeToKeep,omitempty"`
	MaxSegmentCountToKeep      int64        `yaml:"maxSegmentCountToKeep,omitempty"`
}

// ByteQuantity accepts integer bytes or exact decimal quantities with SI/IEC units.
type ByteQuantity int64

var byteQuantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([KMGTPE]i?B?|B)?$`)

func (q *ByteQuantity) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || (node.Tag != "!!str" && node.Tag != "!!int") {
		return errors.New("byte quantity must be an integer or a quantity string")
	}
	parts := byteQuantityPattern.FindStringSubmatch(node.Value)
	if parts == nil {
		return fmt.Errorf("invalid byte quantity %q", node.Value)
	}
	value, ok := new(big.Rat).SetString(parts[1])
	if !ok {
		return fmt.Errorf("invalid byte quantity %q", node.Value)
	}
	unit := strings.TrimSuffix(parts[2], "B")
	base := int64(1000)
	if strings.HasSuffix(unit, "i") {
		base = 1024
		unit = strings.TrimSuffix(unit, "i")
	}
	factor := big.NewInt(1)
	if unit != "" {
		for i := 0; i <= strings.Index("KMGTPE", unit); i++ {
			factor.Mul(factor, big.NewInt(base))
		}
	}
	value.Mul(value, new(big.Rat).SetInt(factor))
	if !value.IsInt() || !value.Num().IsInt64() || value.Sign() <= 0 {
		return fmt.Errorf("byte quantity %q must be positive whole bytes within int64", node.Value)
	}
	*q = ByteQuantity(value.Num().Int64())
	return nil
}

func DefaultLoggingConfig() LoggingConfig {
	return LoggingConfig{Directory: DefaultLogDirectory, Server: LogConfig{WriterType: "stderr", MinLogLevel: "debug"}, Proxy: LogConfig{WriterType: "stderr", MinLogLevel: "info"}, AccessLog: AccessLogConfig{Enabled: true, WriterType: "stderr"}}
}

func LoadLoggingConfig(path string) (LoggingConfig, error) {
	cfg := DefaultLoggingConfig()
	if path == "" {
		return cfg, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	// Inspect nodes before decoding to reject null sections and explicitly nonpositive limits.
	var doc yaml.Node
	if err = decoder.Decode(&doc); err != nil {
		return cfg, fmt.Errorf("logging config: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return cfg, errors.New("logging config must be a mapping")
	}
	if err = validateLoggingNode(doc.Content[0]); err != nil {
		return cfg, err
	}
	encoded, err := yaml.Marshal(&doc)
	if err != nil {
		return cfg, err
	}
	strict := yaml.NewDecoder(strings.NewReader(string(encoded)))
	strict.KnownFields(true)
	if err = strict.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("logging config: %w", err)
	}
	var extra yaml.Node
	if err = decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple YAML documents")
		}
		return cfg, fmt.Errorf("logging config: %w", err)
	}
	return cfg, cfg.Validate()
}

func validateLoggingNode(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		if value.Tag == "!!null" {
			return fmt.Errorf("logging config %s cannot be null", key)
		}
		if key == "rotationPeriodMilliseconds" || key == "maxSegmentCountToKeep" {
			var limit int64
			if value.Tag != "!!int" || value.Decode(&limit) != nil || limit <= 0 {
				return fmt.Errorf("%s must be a positive integer", key)
			}
		}
		if err := validateLoggingNode(value); err != nil {
			return err
		}
	}
	return nil
}

func (c LoggingConfig) Validate() error {
	if c.Directory == "" || !filepath.IsAbs(c.Directory) {
		return errors.New("logging directory must be an absolute path")
	}
	for _, item := range []struct {
		name, writer, level string
		policy              *RotationPolicy
	}{
		{"server", c.Server.WriterType, c.Server.MinLogLevel, c.Server.RotationPolicy},
		{"proxy", c.Proxy.WriterType, c.Proxy.MinLogLevel, c.Proxy.RotationPolicy},
		{"accessLog", c.AccessLog.WriterType, "", c.AccessLog.RotationPolicy},
	} {
		if item.writer != "stdout" && item.writer != "stderr" && item.writer != "file" {
			return fmt.Errorf("%s: invalid writerType %q", item.name, item.writer)
		}
		if item.name != "accessLog" {
			if _, ok := logLevels[item.level]; !ok {
				return fmt.Errorf("%s: invalid minLogLevel %q", item.name, item.level)
			}
		}
		if item.writer != "file" {
			if item.policy != nil {
				return fmt.Errorf("%s: rotationPolicy requires file writer", item.name)
			}
			continue
		}
		p := item.policy
		if p == nil {
			return fmt.Errorf("%s: file writer requires rotationPolicy", item.name)
		}
		if p.RotationPeriodMilliseconds < 0 || p.RotationPeriodMilliseconds > math.MaxInt64/int64(time.Millisecond) || p.MaxSegmentSize < 0 || p.MaxTotalSizeToKeep < 0 || p.MaxSegmentCountToKeep < 0 {
			return fmt.Errorf("%s: invalid rotation limit", item.name)
		}
		if p.RotationPeriodMilliseconds == 0 && p.MaxSegmentSize == 0 {
			return fmt.Errorf("%s: rotation requires period or size trigger", item.name)
		}
		if p.MaxTotalSizeToKeep == 0 && p.MaxSegmentCountToKeep == 0 {
			return fmt.Errorf("%s: rotation requires count or total retention limit", item.name)
		}
	}
	return nil
}

func logPath(directory, name, writer string) string {
	switch writer {
	case "stdout":
		return "/dev/stdout"
	case "stderr":
		return "/dev/stderr"
	default:
		return filepath.Join(directory, name)
	}
}
func (c LoggingConfig) ServerLogPath() string {
	return logPath(c.Directory, "server.log", c.Server.WriterType)
}
func (c LoggingConfig) ProxyLogPath() string {
	return logPath(c.Directory, "envoy.log", c.Proxy.WriterType)
}
func (c LoggingConfig) AccessLogPath() string {
	return logPath(c.Directory, "access.log", c.AccessLog.WriterType)
}
