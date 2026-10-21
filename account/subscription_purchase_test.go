package account

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPurchaseResponseUnmarshalPlan(t *testing.T) {
	var fromString PurchaseResponse
	require.NoError(t, json.Unmarshal([]byte(`{"status":"ok","provider":"web-license","plan":"1y-usd-10","isPro":true}`), &fromString))
	require.Equal(t, "ok", fromString.Status)
	require.Equal(t, "1y-usd-10", fromString.Plan.Id)

	var fromObject PurchaseResponse
	require.NoError(t, json.Unmarshal([]byte(`{"status":"ok","paymentStatus":"done","plan":{"id":"1y-usd-10","usdPrice":1000}}`), &fromObject))
	require.Equal(t, "done", fromObject.PaymentStatus)
	require.Equal(t, "1y-usd-10", fromObject.Plan.Id)
	require.EqualValues(t, 1000, fromObject.Plan.UsdPrice)

	var withError PurchaseResponse
	require.NoError(t, json.Unmarshal([]byte(`{"error":"bad code","errorId":"reseller-codes"}`), &withError))
	require.NotNil(t, withError.BaseResponse)
	require.Equal(t, "bad code", withError.Error)
}
