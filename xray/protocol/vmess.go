package protocol

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
)

type VmessConfig struct {
	V    string      `json:"v"`
	Ps   string      `json:"ps"`
	Add  string      `json:"add"`
	Port json.Number `json:"port"`
	Id   string      `json:"id"`
	Aid  json.Number `json:"aid"`
	Scy  string      `json:"scy"`
	Net  string      `json:"net"`
	Type string      `json:"type"`
	Host string      `json:"host"`
	Path string      `json:"path"`
	Tls  string      `json:"tls"`
	Sni  string      `json:"sni"`
}

var vmessUriRe = regexp.MustCompile(`(?m)vmess://(.+)`)

func ParseVmessUri(uri string) (*VmessConfig, error) {
	sm := vmessUriRe.FindStringSubmatch(uri)
	smString := sm[1]
	//base64 后面补0
	//TODO 有一个这里还是不行
	for len(smString)%3 != 0 {
		smString = smString + "="
	}
	cfgBytes, err := base64.StdEncoding.DecodeString(smString)
	if err != nil {
		return nil, err
	}

	cfg := new(VmessConfig)
	if err = json.Unmarshal(cfgBytes, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
