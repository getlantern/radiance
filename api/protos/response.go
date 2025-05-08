package protos

type SubscriptionType string

const (
	SubscriptionTypeOneTime SubscriptionType = "one_time"
	SubscriptionTypeMonthly SubscriptionType = "monthly"
	SubscriptionTypeYearly  SubscriptionType = "yearly"
)

type SubscriptionPaymentRedirectRequest struct {
	Provider         string           `json:"provider" validate:"required"`
	Plan             string           `json:"plan" validate:"required"`
	DeviceName       string           `json:"deviceName" validate:"required" errorId:"device-name"`
	Email            string           `json:"email"`
	SubscriptionType SubscriptionType `json:"subscriptionType"`
}

type SubscriptionPaymentRedirectResponse struct {
	Redirect string `json:"redirect"`
}

type UserDataResponse struct {
	*BaseResponse           `json:",inline"`
	*LoginResponse_UserData `json:",inline"`
}

type SubscriptionRequest struct {
	Email   string `json:"email" validate:"required"`
	Name    string `json:"name" validate:"required"`
	PriceId string `json:"priceId" validate:"required"`
}

type SubscriptionResponse struct {
	CustomerId     string `json:"customerId"`
	SubscriptionId string `json:"subscriptionId"`
	ClientSecret   string `json:"clientSecret"`
}

type PlansResponse struct {
	*BaseResponse `json:",inline"`
	Providers     map[string][]*PaymentMethod `json:"providers"`
	Plans         []*Plan                     `json:"plans"`
}
