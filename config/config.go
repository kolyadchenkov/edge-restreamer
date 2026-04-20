package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Camera   CameraConfig   `yaml:"camera"`
	Restream RestreamConfig `yaml:"restream"`
	Storage  StorageConfig  `yaml:"storage"`
	Metrics  MetricsConfig  `yaml:"metrics"`
}

type CameraConfig struct {
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
}

type RestreamConfig struct {
	HLS HLSConfig `yaml:"hls"`
	SRT SRTConfig `yaml:"srt"`
}

type HLSConfig struct {
	SegmentDuration time.Duration `yaml:"segment_duration"`
	PlaylistEntries int           `yaml:"playlist_entries"`
	TargetPath      string        `yaml:"target_path"`
}

type SRTConfig struct {
	Host    string        `yaml:"host"`
	Latency time.Duration `yaml:"latency"`
}

type StorageConfig struct {
	S3Endpoint string `yaml:"s3_endpoint"`
	Bucket     string `yaml:"bucket"`
	AccessKey  string `yaml:"access_key"`
	SecretKey  string `yaml:"secret_key"`
}

type MetricsConfig struct {
	WSPort         int           `yaml:"ws_port"`
	ReportInterval time.Duration `yaml:"report_interval"`
}

func LoadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var cfg Config
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
