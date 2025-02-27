package proxyless

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/transport/split"
	"github.com/Jigsaw-Code/outline-sdk/x/configurl"
)

func registerSplitStreamDialer(r configurl.TypeRegistry[transport.StreamDialer], typeID string, newSD configurl.BuildFunc[transport.StreamDialer]) {
	r.RegisterType(typeID, func(ctx context.Context, config *configurl.Config) (transport.StreamDialer, error) {
		sd, err := newSD(ctx, config.BaseConfig)
		if err != nil {
			return nil, err
		}
		configText := config.URL.Opaque
		splits := make([]split.RepeatedSplit, 0)
		for _, part := range strings.Split(configText, ",") {
			var count int
			var bytes int64
			subparts := strings.Split(strings.TrimSpace(part), "*")
			switch len(subparts) {
			case 1:
				count = 1
				bytes, err = strconv.ParseInt(subparts[0], 10, 64)
				if err != nil {
					return nil, fmt.Errorf("bytes is not a number: %v", subparts[0])
				}
			case 2:
				count, err = strconv.Atoi(subparts[0])
				if err != nil {
					return nil, fmt.Errorf("count is not a number: %v", subparts[0])
				}
				bytes, err = strconv.ParseInt(subparts[1], 10, 64)
				if err != nil {
					return nil, fmt.Errorf("bytes is not a number: %v", subparts[1])
				}
			default:
				return nil, fmt.Errorf("split format must be a comma-separated list of '[$COUNT*]$BYTES' (e.g. '100,5*2'). Got %v", part)
			}
			splits = append(splits, split.RepeatedSplit{Count: count, Bytes: bytes})
		}
		return split.NewStreamDialer(sd, split.NewRepeatedSplitIterator(splits...))
	})
}
