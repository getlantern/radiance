package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/ipc"
)

type SubscriptionCmd struct {
	Plans         *SubscriptionPlansCmd   `arg:"subcommand:plans" help:"list subscription plans for a channel"`
	Activate      *ActivateCmd            `arg:"subcommand:activate" help:"activate with reseller code"`
	StripeSub     *StripeSubCmd           `arg:"subcommand:stripe-sub" help:"create Stripe subscription"`
	Redirect      *PaymentRedirectCmd     `arg:"subcommand:redirect" help:"get payment redirect URL"`
	SubRedirect   *SubPaymentRedirectCmd  `arg:"subcommand:sub-redirect" help:"get subscription payment redirect URL"`
	Referral      *ReferralCmd            `arg:"subcommand:referral" help:"attach referral code"`
	StripeBilling *StripeBillingCmd       `arg:"subcommand:stripe-billing" help:"get Stripe billing portal URL"`
	Verify        *VerifySubscriptionCmd  `arg:"subcommand:verify" help:"verify subscription"`
}

type SubscriptionPlansCmd struct {
	Channel string `arg:"--channel" help:"subscription channel"`
}

type ActivateCmd struct {
	Email string `arg:"--email" help:"email address"`
	Code  string `arg:"--code" help:"reseller code"`
}

type StripeSubCmd struct {
	Email  string `arg:"--email" help:"email address"`
	PlanID string `arg:"--plan" help:"plan ID"`
}

type PaymentRedirectCmd struct {
	PlanID      string `arg:"--plan" help:"plan ID"`
	Provider    string `arg:"--provider" help:"payment provider"`
	Email       string `arg:"--email" help:"email address"`
	DeviceName  string `arg:"--device" help:"device name"`
	BillingType string `arg:"--billing-type" default:"subscription" help:"one_time or subscription"`
}

type SubPaymentRedirectCmd struct {
	PlanID      string `arg:"--plan" help:"plan ID"`
	Provider    string `arg:"--provider" help:"payment provider"`
	Email       string `arg:"--email" help:"email address"`
	DeviceName  string `arg:"--device" help:"device name"`
	BillingType string `arg:"--billing-type" default:"subscription" help:"one_time or subscription"`
}

type ReferralCmd struct {
	Code string `arg:"--code" help:"referral code"`
}

type StripeBillingCmd struct {
	BaseURL  string `arg:"--base-url" help:"base URL"`
	UserID   string `arg:"--user-id" help:"user ID"`
	ProToken string `arg:"--token" help:"pro token"`
}

type VerifySubscriptionCmd struct {
	Service    string `arg:"--service" help:"stripe, apple, or google"`
	VerifyData string `arg:"--data" help:"verification data as JSON"`
}

func runSubscription(ctx context.Context, c *ipc.Client, cmd *SubscriptionCmd) error {
	switch {
	case cmd.Plans != nil:
		return subPlans(ctx, c, cmd.Plans)
	case cmd.Activate != nil:
		return subActivate(ctx, c, cmd.Activate)
	case cmd.StripeSub != nil:
		return subStripeSub(ctx, c, cmd.StripeSub)
	case cmd.Redirect != nil:
		return subRedirect(ctx, c, cmd.Redirect)
	case cmd.SubRedirect != nil:
		return subSubRedirect(ctx, c, cmd.SubRedirect)
	case cmd.Referral != nil:
		return subReferral(ctx, c, cmd.Referral)
	case cmd.StripeBilling != nil:
		return subStripeBilling(ctx, c, cmd.StripeBilling)
	case cmd.Verify != nil:
		return subVerify(ctx, c, cmd.Verify)
	default:
		return fmt.Errorf("no subcommand specified")
	}
}

func subPlans(ctx context.Context, c *ipc.Client, cmd *SubscriptionPlansCmd) error {
	channel := cmd.Channel
	if channel == "" {
		var err error
		channel, err = prompt("Channel: ")
		if err != nil {
			return err
		}
	}
	plans, err := c.SubscriptionPlans(ctx, channel)
	if err != nil {
		return err
	}
	fmt.Println(plans)
	return nil
}

func subActivate(ctx context.Context, c *ipc.Client, cmd *ActivateCmd) error {
	email := cmd.Email
	code := cmd.Code
	var err error
	if email == "" {
		email, err = prompt("Email: ")
		if err != nil {
			return err
		}
	}
	if code == "" {
		code, err = prompt("Reseller code: ")
		if err != nil {
			return err
		}
	}
	resp, err := c.ActivationCode(ctx, email, code)
	if err != nil {
		return err
	}
	return printJSON(resp)
}

func subStripeSub(ctx context.Context, c *ipc.Client, cmd *StripeSubCmd) error {
	email := cmd.Email
	planID := cmd.PlanID
	var err error
	if email == "" {
		email, err = prompt("Email: ")
		if err != nil {
			return err
		}
	}
	if planID == "" {
		planID, err = prompt("Plan ID: ")
		if err != nil {
			return err
		}
	}
	secret, err := c.NewStripeSubscription(ctx, email, planID)
	if err != nil {
		return err
	}
	fmt.Println(secret)
	return nil
}

func promptRedirectData(planID, provider, email, deviceName, billingType string) (account.PaymentRedirectData, error) {
	var err error
	if planID == "" {
		planID, err = prompt("Plan ID: ")
		if err != nil {
			return account.PaymentRedirectData{}, err
		}
	}
	if provider == "" {
		provider, err = prompt("Provider: ")
		if err != nil {
			return account.PaymentRedirectData{}, err
		}
	}
	if email == "" {
		email, err = prompt("Email: ")
		if err != nil {
			return account.PaymentRedirectData{}, err
		}
	}
	if deviceName == "" {
		deviceName, err = prompt("Device name: ")
		if err != nil {
			return account.PaymentRedirectData{}, err
		}
	}
	if billingType == "" {
		billingType = "subscription"
	}
	return account.PaymentRedirectData{
		Plan:        planID,
		Provider:    provider,
		Email:       email,
		DeviceName:  deviceName,
		BillingType: account.SubscriptionType(billingType),
	}, nil
}

func subRedirect(ctx context.Context, c *ipc.Client, cmd *PaymentRedirectCmd) error {
	data, err := promptRedirectData(cmd.PlanID, cmd.Provider, cmd.Email, cmd.DeviceName, cmd.BillingType)
	if err != nil {
		return err
	}
	url, err := c.PaymentRedirect(ctx, data)
	if err != nil {
		return err
	}
	fmt.Println(url)
	return nil
}

func subSubRedirect(ctx context.Context, c *ipc.Client, cmd *SubPaymentRedirectCmd) error {
	data, err := promptRedirectData(cmd.PlanID, cmd.Provider, cmd.Email, cmd.DeviceName, cmd.BillingType)
	if err != nil {
		return err
	}
	url, err := c.SubscriptionPaymentRedirectURL(ctx, data)
	if err != nil {
		return err
	}
	fmt.Println(url)
	return nil
}

func subReferral(ctx context.Context, c *ipc.Client, cmd *ReferralCmd) error {
	code := cmd.Code
	if code == "" {
		var err error
		code, err = prompt("Referral code: ")
		if err != nil {
			return err
		}
	}
	ok, err := c.ReferralAttach(ctx, code)
	if err != nil {
		return err
	}
	if ok {
		fmt.Println("Referral attached successfully")
	} else {
		fmt.Println("Referral was not attached")
	}
	return nil
}

func subStripeBilling(ctx context.Context, c *ipc.Client, cmd *StripeBillingCmd) error {
	baseURL := cmd.BaseURL
	userID := cmd.UserID
	proToken := cmd.ProToken
	var err error
	if baseURL == "" {
		baseURL, err = prompt("Base URL: ")
		if err != nil {
			return err
		}
	}
	if userID == "" {
		userID, err = prompt("User ID: ")
		if err != nil {
			return err
		}
	}
	if proToken == "" {
		proToken, err = prompt("Pro token: ")
		if err != nil {
			return err
		}
	}
	url, err := c.StripeBillingPortalURL(ctx, baseURL, userID, proToken)
	if err != nil {
		return err
	}
	fmt.Println(url)
	return nil
}

func subVerify(ctx context.Context, c *ipc.Client, cmd *VerifySubscriptionCmd) error {
	service := cmd.Service
	verifyData := cmd.VerifyData
	var err error
	if service == "" {
		service, err = prompt("Service (stripe, apple, or google): ")
		if err != nil {
			return err
		}
	}
	if verifyData == "" {
		verifyData, err = prompt("Verification data (JSON): ")
		if err != nil {
			return err
		}
	}
	var data map[string]string
	if err := json.Unmarshal([]byte(verifyData), &data); err != nil {
		return fmt.Errorf("invalid JSON for verification data: %w", err)
	}
	result, err := c.VerifySubscription(ctx, account.SubscriptionService(service), data)
	if err != nil {
		return err
	}
	fmt.Println(result)
	return nil
}
