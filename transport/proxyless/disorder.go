package proxyless

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/configurl"
	"github.com/Jigsaw-Code/outline-sdk/x/disorder"
)

func registerDisorderDialer(r configurl.TypeRegistry[transport.StreamDialer], typeID string, newSD configurl.BuildFunc[transport.StreamDialer]) {
	r.RegisterType(typeID, func(ctx context.Context, config *configurl.Config) (transport.StreamDialer, error) {
		sd, err := newSD(ctx, config.BaseConfig)
		if err != nil {
			return nil, err
		}
		disorderPacketNStr := config.URL.Opaque
		disorderPacketN, err := strconv.Atoi(disorderPacketNStr)
		if err != nil {
			return nil, fmt.Errorf("disoder: could not parse splice position: %v", err)
		}
		return disorder.NewStreamDialer(sd, disorderPacketN)
	})
}
