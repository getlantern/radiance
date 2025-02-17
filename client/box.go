package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getlantern/golog"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

var glog = golog.LoggerFor("box")

func NewBox() (*box.Box, error) {
	glog.Debug("Creating box")
	opts := getopts()

	glog.Debug("Config loaded")
	// fmt.Printf("rules: %+v\n", opts.DNS.Rules)
	// save(opts)

	// ***** REGISTER NEW PROTOCOL HERE *****
	ctx := box.Context(
		context.Background(),
		include.InboundRegistry(),
		include.OutboundRegistry(),
		include.EndpointRegistry(),
	)
	glog.Debug("registering algeneva protocol")
	outboundRegistry := service.FromContext[adapter.OutboundRegistry](ctx)
	RegisterOutbound(outboundRegistry.(*outbound.Registry))
	// see https://github.com/SagerNet/sing-box/blob/v1.11.3/protocol/http/outbound.go#L22

	boxOpts := box.Options{
		Options: opts,
		Context: ctx,
	}
	return box.New(boxOpts)
}

func loadConfig() (*option.Options, error) {
	path := filepath.Join("client", "test.json")
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}
	opts, err := json.UnmarshalExtended[option.Options](buf)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %v", err)
	}
	return &opts, nil
}
