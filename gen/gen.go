package gen

import (
	"encoding/base64"
	"encoding/json"
	"github.com/Bpazy/xraysub/xray"
	"github.com/Bpazy/xraysub/xray/protocol"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"io/ioutil"
	"strings"
)

type CmdConfig struct {
	Url        string
	OutputFile string
}

var Cfg = &CmdConfig{}

func (c CmdConfig) GetOutputFile() string {
	if c.OutputFile != "" {
		return c.OutputFile
	}
	// default output file
	return "./xray-config.json"
}

func NewGenCmdRun() func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		c := resty.New()
		res, err := c.R().Get(Cfg.Url)
		cobra.CheckErr(err)

		dst, err := base64.StdEncoding.DecodeString(res.String())
		cobra.CheckErr(err)

		uris := strings.Split(strings.TrimSpace(string(dst)), "\n")
		links := parseLinks(uris)

		// pretty print
		for _, cfg := range links {
			j, _ := json.Marshal(cfg)
			log.Printf("Shadowsocks cfg: %s", string(j))
		}

		xraycfg := getXrayConfig(links)
		cfg, err := json.Marshal(xraycfg)
		cobra.CheckErr(err)
		err = ioutil.WriteFile(Cfg.GetOutputFile(), cfg, 0644)
		cobra.CheckErr(err)
	}
}

func parseLinks(uris []string) []*Link {
	var links []*Link
	for _, uri := range uris {
		p, err := protocol.GetProtocol(uri)
		if err != nil {
			log.Warn("unrecognized protocol: " + uri)
			continue
		}

		switch p {
		case protocol.Shadowsocks:
			cfg, err := protocol.ParseShadowsocksUri(uri)
			if err != nil {
				log.Warn("illegal shadowsocks uri schema: " + uri)
				continue
			}
			links = append(links, &Link{
				SsCfg: cfg,
			})
		case protocol.Vmess:

		}
	}
	return links
}

type Link struct {
	SsCfg *protocol.ShadowsocksConfig
}

func getXrayConfig(links []*Link) *xray.XrayConfig {
	return &xray.XrayConfig{
		Policy: &xray.Policy{
			System: xray.System{
				StatsOutboundUplink:   true,
				StatsOutboundDownlink: true,
			},
		},
		Log: &xray.Log{
			Access:   "",
			Error:    "",
			Loglevel: "warning",
		},
		Inbounds:  getInbounds(),
		Outbounds: getOutBounds(links),
		Routing: &xray.Routing{
			DomainStrategy: "IPIfNonMatch",
			DomainMatcher:  "linear",
			Rules: []*xray.Rule{
				{
					Type:        "field",
					OutboundTag: "proxy",
					Port:        "0-65535",
				},
			},
		},
	}
}

func getOutBounds(links []*Link) []*xray.ShadowsocksOutbound {
	var outbounds []*xray.ShadowsocksOutbound
	for _, link := range links {
		outbounds = append(outbounds, &xray.ShadowsocksOutbound{
			BaseOutbound: xray.BaseOutbound{
				Tag:      "proxy", // 应该测速后选择最合适的设置 tag 为 proxy
				Protocol: "shadowsocks",
			},
			Settings: &xray.OutboundSettings{
				Servers: []*xray.ShadowsocksServer{
					{
						Address:  link.SsCfg.Hostname,
						Method:   link.SsCfg.Method,
						Ota:      false,
						Password: link.SsCfg.Password,
						Port:     link.SsCfg.Port,
						Level:    1,
					},
				},
			},
			StreamSettings: &xray.StreamSettings{
				Network: "tcp",
			},
			Mux: &xray.Mux{
				Enabled:     false,
				Concurrency: -1,
			},
		})
	}
	return outbounds
}

func getInbounds() []*xray.Inbound {
	return []*xray.Inbound{
		{
			Tag:      "socks",
			Port:     10808,
			Listen:   "0.0.0.0",
			Protocol: "socks",
			Sniffing: &xray.Sniffing{
				Enabled:      true,
				DestOverride: []string{"http", "tls"},
			},
			Settings: &xray.InboundSettings{
				Auth:             "noauth",
				Udp:              true,
				AllowTransparent: false,
			},
		},
		{
			Tag:      "http",
			Port:     10809,
			Listen:   "0.0.0.0",
			Protocol: "http",
			Sniffing: &xray.Sniffing{
				Enabled:      true,
				DestOverride: []string{"http", "tls"},
			},
			Settings: &xray.InboundSettings{
				Udp:              false,
				AllowTransparent: false,
			},
		},
	}
}
