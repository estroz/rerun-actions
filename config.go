package main

import (
	"fmt"
	"io/ioutil"

	"github.com/palantir/go-baseapp/baseapp"
	"github.com/palantir/go-githubapp/githubapp"
	yaml "gopkg.in/yaml.v3"
)

type Config struct {
	AppConfig `json:"app_configuration"`

	Server baseapp.HTTPConfig `yaml:"server"`
	Github githubapp.Config   `yaml:"github"`
}

type AppConfig struct {
	AllowUserRegexpList []string `yaml:"allow_user_regexp_list,omitempty"`
	DenyUserRegexpList  []string `yaml:"deny_user_regexp_list,omitempty"`
}

func readConfig(path string) (*Config, error) {
	var c Config

	bytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed reading server config file: %v", err)
	}

	if err := yaml.Unmarshal(bytes, &c); err != nil {
		return nil, fmt.Errorf("failed parsing configuration file: %v", err)
	}

	return &c, nil
}
