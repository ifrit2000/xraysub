package gen

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Bpazy/xraysub/constants"
	"github.com/Bpazy/xraysub/util"
	"github.com/Bpazy/xraysub/xray"
	"github.com/Bpazy/xraysub/xray/protocol"
	"github.com/go-resty/resty/v2"
	"github.com/schollz/progressbar/v3"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CmdConfig config for command: xraysub gen
type CmdConfig struct {
	Url                string // subscription link
	OutputFile         string // xray-core's configuration path
	DetectLatency      bool   // detect latency to select the best server
	DetectUrl          string // detect latency url
	DetectThreadNumber int    // detect latency threads number

	XrayCorePath  string // xray-core path, for some case such as: speed test
	XraySocksPort int    // xray-core listen socks port
	XrayHttpPort  int    // xray-core listen http port
}

var Cfg = &CmdConfig{}

func NewGenCmdRun() func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		c := resty.New()
		fmt.Printf("Requesting subscriptions from %s\n", Cfg.Url)
		c.SetTimeout(5 * time.Second)
		res, err := c.R().Get(Cfg.Url)
		util.CheckErr(err)
		s := res.String()

		var uris []string
		if strings.HasPrefix(s, "vmess://") {
			uris = strings.Split(s, "\n")
		} else {
			dst, err := base64.StdEncoding.DecodeString(s)
			util.CheckErr(err)
			uris = strings.Split(strings.TrimSpace(string(dst)), "\n")
		}

		links := parseLinks(uris)
		xrayCfg := getXrayConfig(links)
		if Cfg.DetectLatency {
			err := detectLatency(xrayCfg)
			util.CheckErr(err)
		}

		writeFile(xrayCfg, Cfg.OutputFile)
	}
}

var xrayCoreProcess *os.Process

func init() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)

	go func() {
		s := <-c
		log.Infof("Got signal: %s", s.String())
		killXrayCoreProcess()
		os.Exit(1)
	}()
}

func killXrayCoreProcess() {
	if xrayCoreProcess != nil {
		log.Infof("kill xray-core, PID: %d", xrayCoreProcess.Pid)
		util.CheckErr(xrayCoreProcess.Kill())
	}
}

// speed test, return the fastest node
func detectLatency(xCfg *xray.Config) error {
	fmt.Println("Start detecting servers latency")
	if len(xCfg.Outbounds) == 0 {
		return errors.New("outbounds empty")
	}
	// generate inbound and routing rules based on outbound to test latency
	var inbounds []*xray.Inbound
	var routingRules []*xray.Rule
	inboundPorts := randomInboundPorts(xCfg.Outbounds)
	for i, outbound := range xCfg.Outbounds {
		inbound := getInboundFromOutbound(i, inboundPorts[i])
		outbound.Inbound = inbound
		inbounds = append(inbounds, inbound)
		routingRules = append(routingRules, getRoutingRules(inbound, outbound))
	}
	oldInbounds := xCfg.Inbounds
	xCfg.Inbounds = inbounds
	xCfg.Routing.Rules = routingRules
	defer func() {
		xCfg.Inbounds = oldInbounds
	}()

	f, err := writeTempConfig(xCfg)
	if err != nil {
		return err
	}
	defer os.RemoveAll(f.Name())

	cmd := exec.Command(Cfg.XrayCorePath, "-c", f.Name(), "-format=json")

	// for:
	//   1. xray-core.log
	//   2. buffer for check xray-core status
	xlf, err := appendXrayCoreLogFile()
	if err != nil {
		return fmt.Errorf("create xray-core.log error: %w", err)
	}
	defer util.Closeq(xlf)
	buf := new(bytes.Buffer)
	w := io.MultiWriter(xlf, buf)

	outPipe, err := cmd.StdoutPipe()
	util.CheckErr(err)

	go func() {
		_, err = io.Copy(w, outPipe)
		util.CheckErr(err)
	}()

	if err = cmd.Start(); err != nil {
		return fmt.Errorf("exec xray-core error: %w", err)
	}
	log.Infof("xray-core PID: %d", cmd.Process.Pid)
	xrayCoreProcess = cmd.Process
	defer killXrayCoreProcess()

	// start rendering progress bar
	bar := initProgressBar(xCfg)

	if err = checkXrayCoreStatus(buf); err != nil {
		return err
	}

	wg := new(sync.WaitGroup)
	outboundChan := make(chan *xray.OutBound)
	for i := 0; i < Cfg.DetectThreadNumber; i++ {
		wg.Add(1)
		go detectWorker(outboundChan, wg, bar)
	}
	for _, outbound := range xCfg.Outbounds {
		outboundChan <- outbound
	}
	close(outboundChan)
	wg.Wait()
	fmt.Println()

	// filter fasted outbound
	fastedOutbound, err := getFastedOutbound(xCfg)
	if err != nil {
		return err
	}
	xCfg.Routing.Rules = []*xray.Rule{
		{
			Type:        "field",
			OutboundTag: fastedOutbound.Tag,
			Port:        "0-65535",
		},
	}

	return nil
}

func getFastedOutbound(xCfg *xray.Config) (*xray.OutBound, error) {
	var fastedOutbound *xray.OutBound
	for _, outbound := range xCfg.Outbounds {
		if outbound.Latency == nil {
			continue
		}
		if fastedOutbound == nil {
			fastedOutbound = outbound
		} else if fastedOutbound.Latency.Milliseconds() > outbound.Latency.Milliseconds() {
			fastedOutbound = outbound
		}
	}
	if fastedOutbound == nil {
		return nil, errors.New("all nodes detectLatency test failed")
	} else {
		fmt.Printf("Got fastest node \"%s\" with latency %dms\n", fastedOutbound.PrettyComment(), fastedOutbound.Latency.Milliseconds())
	}
	return fastedOutbound, nil
}

// check xray-core started status
func checkXrayCoreStatus(buf *bytes.Buffer) error {
	// wait up to 3 seconds for Xray to start
	timeout := time.After(3 * time.Second)
LOOP:
	for {
		s := buf.String()
		select {
		case <-timeout:
			return errors.New("start xray-core error, please check xray-core's log")
		default:
			if strings.Contains(s, "started") {
				break LOOP
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

func initProgressBar(xCfg *xray.Config) *progressbar.ProgressBar {
	return progressbar.NewOptions(len(xCfg.Outbounds),
		progressbar.OptionSetDescription("\tDetecting"),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}))
}

func appendXrayCoreLogFile() (*os.File, error) {
	f, err := os.OpenFile("xray-core.log", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	return f, err
}

func detectWorker(oc chan *xray.OutBound, wg *sync.WaitGroup, bar *progressbar.ProgressBar) {
	defer wg.Done()

	for outbound := range oc {
		client := resty.New()
		proxy := "http://127.0.0.1:" + strconv.Itoa(outbound.Inbound.Port)
		client.SetProxy(proxy)
		client.SetTimeout(5 * time.Second)
		start := time.Now()
		_, err := client.R().Get(Cfg.DetectUrl)
		if err != nil {
			log.Errorf("request failed by proxy %s: %+v", proxy, err)
		} else {
			since := time.Since(start)
			outbound.Latency = &since
			ap := outbound.Settings.GetAddressPort()
			log.Infof("%s:%d cost %dms", ap.GetAddress(), ap.GetPort(), outbound.Latency.Milliseconds())
		}
		_ = bar.Add(1)
	}
}

func randomInboundPorts(outbounds []*xray.OutBound) []int {
	var ports []int
	offset := 0
	for range outbounds {
		for {
			p := 40000 + offset
			offset++
			listenable := portListenable(p)
			if listenable {
				ports = append(ports, p)
				break
			}
		}
	}
	return ports
}

func portListenable(p int) bool {
	listen, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(p))
	if err == nil {
		// 端口可用
		_ = listen.Close()
		return true
	}
	return false
}

func writeTempConfig(xCfg *xray.Config) (*os.File, error) {
	j, err := json.Marshal(xCfg)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(os.TempDir(), "xray.config.json")
	if err != nil {
		return nil, fmt.Errorf("create temp file 'xray.config.json' error: %w", err)
	}
	_, err = f.Write(j)
	if err != nil {
		return nil, fmt.Errorf("write to temp file 'xray.config.json' error: %w", err)
	}
	return f, nil
}

func getRoutingRules(inbound *xray.Inbound, outbound *xray.OutBound) *xray.Rule {
	return &xray.Rule{
		Type:        "field",
		InboundTag:  []string{inbound.Tag},
		OutboundTag: outbound.Tag,
	}
}

func getInboundFromOutbound(i int, port int) *xray.Inbound {
	return &xray.Inbound{
		Tag:      "inbound" + strconv.Itoa(i),
		Port:     port,
		Listen:   constants.ListenAllAddress,
		Protocol: "http",
		Sniffing: &xray.Sniffing{
			Enabled:      true,
			DestOverride: []string{"http", "tls"},
		},
		Settings: &xray.InboundSettings{
			Udp:              false,
			AllowTransparent: false,
		},
	}
}

// write xray-core's configuration
func writeFile(cfg *xray.Config, path string) {
	j, err := json.Marshal(cfg)
	util.CheckErr(err)
	err = os.WriteFile(path, j, 0644)
	util.CheckErr(err)
	fmt.Printf("The xray-core's configuration file is saved %s\n", path)
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
		//不用ss类型的
		//case protocol.Shadowsocks:
		//	cfg, err := protocol.ParseShadowsocksUri(uri)
		//	if err != nil {
		//		log.Warn("illegal shadowsocks uri schema: " + uri)
		//		continue
		//	}
		//	links = append(links, &Link{
		//		SsCfg: cfg,
		//	})
		case protocol.Vmess:
			cfg, err := protocol.ParseVmessUri(uri)
			if err != nil {
				log.Warn("illegal vmess uri schema: " + uri)
				continue
			}
			//只用第三或者第四个
			if strings.Contains(cfg.Ps, "3.") || strings.Contains(cfg.Ps, "4.") {
				links = append(links, &Link{
					VmessCfg: cfg,
				})
			} else {
				continue
			}
		case protocol.Trojan:
			cfg, err := protocol.ParseTrojanUri(uri)
			if err != nil {
				log.Warn("illegal vmess uri schema: " + uri)
				continue
			}
			links = append(links, &Link{
				TrojanCfg: cfg,
			})
		}
	}
	return links
}

type Link struct {
	SsCfg     *protocol.ShadowsocksConfig
	VmessCfg  *protocol.VmessConfig
	TrojanCfg *protocol.TrojanConfig
}

// build xray-core config
func getXrayConfig(links []*Link) *xray.Config {
	return &xray.Config{
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
		},
	}
}

func getOutBounds(links []*Link) []*xray.OutBound {
	var outbounds []*xray.OutBound
	for i, link := range links {
		outbounds = append(outbounds, &xray.OutBound{
			Tag:            "outbound" + strconv.Itoa(i), // 应该测速后选择最合适的设置 tag 为 proxy
			Protocol:       getOutboundProtocol(link),
			Comment:        getOutboundComment(link),
			Settings:       getOutboundSettings(link),
			StreamSettings: getOutboundStreamSettings(link),
			Mux: &xray.Mux{
				Enabled:     false,
				Concurrency: -1,
			},
		})
	}
	return outbounds
}

func getOutboundStreamSettings(link *Link) *xray.StreamSettings {
	if link.SsCfg != nil || link.TrojanCfg != nil {
		return &xray.StreamSettings{
			Network:  "tcp",
			Security: getOutboundStreamSettingsSecurity(link),
		}
	}
	if link.VmessCfg != nil {
		return &xray.StreamSettings{
			Network:    link.VmessCfg.Net,
			WsSettings: nil,
		}
	}
	// TODO error handle
	panic(fmt.Sprintf("Not supported protocol: %+v", link))
}

func getOutboundStreamSettingsSecurity(link *Link) string {
	if link.SsCfg != nil {
		return ""
	}
	return "tls"
}

func getOutboundComment(link *Link) string {
	if link.SsCfg != nil {
		return link.SsCfg.Comment
	}
	if link.TrojanCfg != nil {
		return ""
	}
	return link.VmessCfg.Ps
}

func getOutboundSettings(link *Link) *xray.OutboundSettings {
	s := new(xray.OutboundSettings)
	if link.SsCfg != nil {
		s.Servers = []interface{}{
			&xray.ShadowsocksServer{
				Address:  link.SsCfg.Hostname,
				Method:   link.SsCfg.Method,
				Ota:      false,
				Password: link.SsCfg.Password,
				Port:     link.SsCfg.Port,
				Level:    1,
			},
		}
	} else if link.VmessCfg != nil {
		p, err := link.VmessCfg.Port.Int64()
		if err != nil {
			util.CheckErr(err)
		}
		aid, err := link.VmessCfg.Aid.Int64()
		if err != nil {
			util.CheckErr(err)
		}
		s.Vnext = []*xray.Vnext{
			{
				Address: link.VmessCfg.Add,
				Port:    int(p),
				Users: []xray.User{
					{
						Id:       link.VmessCfg.Id,
						AlterId:  int(aid),
						Email:    "",
						Security: link.VmessCfg.Scy,
					},
				},
			},
		}
	} else {
		s.Servers = []interface{}{
			&xray.TrojanServer{
				Address:  link.TrojanCfg.Host,
				Password: link.TrojanCfg.Password,
				Port:     link.TrojanCfg.Port,
			},
		}
	}

	return s
}

func getOutboundProtocol(link *Link) string {
	var p string
	if link.SsCfg != nil {
		p = "shadowsocks"
	} else if link.VmessCfg != nil {
		p = "vmess"
	} else {
		p = "trojan"
	}
	return p
}

func getInbounds() []*xray.Inbound {
	return []*xray.Inbound{
		{
			Tag:      "socks",
			Port:     Cfg.XraySocksPort,
			Listen:   constants.ListenAllAddress,
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
			Port:     Cfg.XrayHttpPort,
			Listen:   constants.ListenAllAddress,
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
