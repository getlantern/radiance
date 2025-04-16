package pro

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/getlantern/radiance/common"
	"github.com/stretchr/testify/assert"
)

func TestSubscriptionPaymentRedirect(t *testing.T) {
	userConfig, _ := common.NewUserConfig("HFJDFJ-75885F", "")

	proServer := New(&http.Client{}, userConfig)
	resp, error := proServer.SubscriptionPaymentRedirect(context.Background(), nil)
	assert.NoError(t, error)
	assert.NotNil(t, resp.Redirect)
	fmt.Printf("SubscriptionPaymentRedirect response: %v", resp)
}

func TestCreateUser(t *testing.T) {
	userConfig, _ := common.NewUserConfig("HFJDFJ-75885F", "")
	proServer := New(&http.Client{}, userConfig)
	resp, error := proServer.UserCreate(context.Background())
	assert.NoError(t, error)
	assert.NotNil(t, resp)
	fmt.Printf("UserCreate response: %v", resp)
}
