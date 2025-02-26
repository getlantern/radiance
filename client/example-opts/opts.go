package opts

import (
	"context"
	"fmt"
	"os"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

func save(opts *option.Options, path string) error {
	b, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}
	return os.WriteFile(path, b, 0644)
}

func load(ctx context.Context, path string) (*option.Options, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}
	opts, err := json.UnmarshalExtendedContext[box.Options](ctx, buf)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %v", err)
	}
	return &opts.Options, nil
}
