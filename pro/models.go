package pro

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
