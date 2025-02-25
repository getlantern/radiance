package proxyless

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/transport/tlsfrag"
	"github.com/Jigsaw-Code/outline-sdk/x/configurl"
)

func registerTLSFragStreamDialer(r configurl.TypeRegistry[transport.StreamDialer], typeID string, newSD configurl.BuildFunc[transport.StreamDialer]) {
	r.RegisterType(typeID, func(ctx context.Context, config *configurl.Config) (transport.StreamDialer, error) {
		sd, err := newSD(ctx, config.BaseConfig)
		if err != nil {
			return nil, err
		}
		lenStr := config.URL.Opaque
		fixedLen, err := strconv.Atoi(lenStr)
		if err != nil {
			return nil, fmt.Errorf("invalid tlsfrag option: %v. It should be in tlsfrag:<number> format", lenStr)
		}
		return tlsfrag.NewFixedLenStreamDialer(sd, fixedLen)
	})
}
