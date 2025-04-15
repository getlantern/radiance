package pro

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSubscriptionPaymentRedirect(t *testing.T) {
	proServer := New(&http.Client{}, "test-device-id")
	resp, error := proServer.SubscriptionPaymentRedirect(context.Background(), nil)
	assert.NoError(t, error)
	assert.NotNil(t, resp.Redirect)
	fmt.Printf("SubscriptionPaymentRedirect response: %v", resp)
}

func TestCreateUser(t *testing.T) {
	proServer := New(&http.Client{}, "test-device-id")
	resp, error := proServer.UserCreate(context.Background())
	assert.NoError(t, error)
	assert.NotNil(t, resp)
	fmt.Printf("UserCreate response: %v", resp)
}
