package pro

// func TestSubscriptionPaymentRedirect(t *testing.T) {
// 	userConfig := common.NewUserConfig("HFJDFJ-75885F", "")
// 	proServer := New(&http.Client{}, userConfig)
// 	body := &protos.SubscriptionPaymentRedirectRequest{
// 		Provider:         "stripe",
// 		Plan:             "pro",
// 		DeviceName:       "test-device",
// 		Email:            "",
// 		SubscriptionType: protos.SubscriptionType("monthly"),
// 	}
// 	resp, error := proServer.SubscriptionPaymentRedirect(context.Background(), body)
// 	assert.NoError(t, error)
// 	assert.NotNil(t, resp.Redirect)
// 	fmt.Printf("SubscriptionPaymentRedirect response: %v", resp)
// }

// func TestCreateUser(t *testing.T) {
// 	userConfig := common.NewUserConfig("HFJDFJ-75885F", "")
// 	proServer := New(&http.Client{}, userConfig)
// 	resp, error := proServer.UserCreate(context.Background())
// 	assert.NoError(t, error)
// 	assert.NotNil(t, resp)
// 	fmt.Printf("UserCreate response: %v", resp)
// }

// func TestStripeSubscription(t *testing.T) {
// 	userConfig := common.NewUserConfig("HFJDFJ-75885F", "")
// 	proServer := New(&http.Client{}, userConfig)

// 	body := &protos.SubscriptionRequest{
// 		Email:   "test@getlantern.org",
// 		Name:    "Test User",
// 		PriceId: "price_1RCg464XJ6zbDKY5T6kqbMC6",
// 	}
// 	resp, error := proServer.StripeSubscription(context.Background(), body)
// 	assert.NoError(t, error)
// 	assert.NotNil(t, resp)
// 	fmt.Printf("Stripe Subscription response: %v", resp)
// }
