package backend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/getlantern/common/usermessage"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/usermessage"
)

type userMessageFetcherFunc func(context.Context, usermessage.ClientContext, []string) (wire.UserMessageResponse, error)

func (f userMessageFetcherFunc) Fetch(ctx context.Context, client usermessage.ClientContext, seen []string) (wire.UserMessageResponse, error) {
	return f(ctx, client, seen)
}

type accountTransportFunc func(*http.Request) (*http.Response, error)

func (f accountTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestPatchSettingsAccountKeysDiscardStaleUserData(t *testing.T) {
	settings.Reset()
	t.Cleanup(settings.Reset)
	require.NoError(t, settings.InitSettings(t.TempDir()))
	require.NoError(t, settings.Patch(settings.Settings{settings.UserIDKey: int64(1), settings.TokenKey: "a"}))
	requested, release := make(chan struct{}), make(chan struct{})
	a := account.NewClient(&http.Client{Transport: accountTransportFunc(func(req *http.Request) (*http.Response, error) {
		close(requested)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"userId":1,"token":"a","userLevel":"pro"}`)),
			Request:    req,
		}, nil
	})}, t.TempDir(), log.NoOpLogger())
	r := &LocalBackend{accountClient: a}
	errc := make(chan error, 1)
	go func() {
		_, err := a.FetchUserData(context.Background())
		errc <- err
	}()
	<-requested

	require.NoError(t, r.PatchSettings(settings.Settings{settings.UserIDKey: int64(2), settings.TokenKey: "b"}))
	close(release)

	require.ErrorIs(t, <-errc, account.ErrAccountChanged)
	require.Equal(t, int64(2), settings.GetInt64(settings.UserIDKey))
	require.False(t, settings.IsPro())
}

func TestPatchSettingsSwitchKeepsMatches(t *testing.T) {
	settings.Reset()
	t.Cleanup(settings.Reset)
	require.NoError(t, settings.InitSettings(t.TempDir()))
	require.NoError(t, settings.Patch(settings.Settings{
		settings.UserIDKey:     int64(1),
		settings.TokenKey:      "a",
		settings.UserLevelKey:  "pro",
		settings.OAuthLoginKey: true,
	}))
	r := &LocalBackend{accountClient: account.NewClient(http.DefaultClient, t.TempDir(), log.NoOpLogger())}

	require.NoError(t, r.PatchSettings(settings.Settings{
		settings.UserIDKey:     int64(2),
		settings.TokenKey:      "b",
		settings.UserLevelKey:  "pro",
		settings.OAuthLoginKey: true,
	}))

	require.Equal(t, int64(2), settings.GetInt64(settings.UserIDKey))
	require.True(t, settings.IsPro())
	require.True(t, settings.GetBool(settings.OAuthLoginKey))
}

func TestUserMessagesFollowAccountEvents(t *testing.T) {
	settings.Reset()
	t.Cleanup(settings.Reset)
	require.NoError(t, settings.InitSettings(t.TempDir()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var userID atomic.Int64
	userID.Store(122)
	a := account.NewClient(&http.Client{Transport: accountTransportFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/user-create") {
			userID.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"userId":%d,"token":"test-token","userLevel":"free"}`, userID.Load()))),
			Request:    req,
		}, nil
	})}, t.TempDir(), log.NoOpLogger())
	var calls atomic.Int32
	contexts := make(chan usermessage.ClientContext, 8)
	checkedCredentials := make(chan struct{})
	var checkedOnce sync.Once
	service, err := usermessage.New(usermessage.Options{
		DataDir: t.TempDir(),
		Logger:  log.NoOpLogger(),
		ContextProvider: func() usermessage.ClientContext {
			client := usermessage.ClientContext{
				UserID:   settings.GetString(settings.UserIDKey),
				ProToken: settings.GetString(settings.TokenKey),
				Locale:   "en-US", Platform: "macos", AppVersion: "9.0.0",
			}
			checkedOnce.Do(func() { close(checkedCredentials) })
			return client
		},
		Fetcher: userMessageFetcherFunc(func(ctx context.Context, client usermessage.ClientContext, seen []string) (wire.UserMessageResponse, error) {
			calls.Add(1)
			contexts <- client
			return wire.UserMessageResponse{
				PollIntervalSeconds: wire.MaxPollIntervalSeconds,
				Message: &wire.ResolvedUserMessage{
					DisplayID: "test-message", CampaignID: "campaign", RevisionID: "revision", DeliveryID: "delivery",
					Surface: wire.SurfaceSnackbar, Locale: "en-US", Body: "Test message", ExpiresAt: time.Now().Add(time.Hour),
				},
			}, nil
		}),
	})
	require.NoError(t, err)
	r := &LocalBackend{ctx: ctx, accountClient: a, userMessages: service}
	r.startUserMessages()
	select {
	case <-checkedCredentials:
	case <-time.After(3 * time.Second):
		t.Fatal("messaging did not check initial credentials")
	}
	require.Zero(t, calls.Load())

	// The config fetcher calls account.Client directly, not LocalBackend.NewUser.
	_, err = a.NewUser(ctx)
	require.NoError(t, err)
	select {
	case client := <-contexts:
		require.Equal(t, "123", client.UserID)
		require.Equal(t, "test-token", client.ProToken)
	case <-time.After(3 * time.Second):
		t.Fatal("initial account creation did not wake messaging")
	}
	require.Eventually(t, func() bool {
		message, err := r.CurrentUserMessage()
		return err == nil && message != nil
	}, 3*time.Second, time.Millisecond)

	_, err = r.FetchUserData(ctx)
	require.NoError(t, err)
	require.Never(t, func() bool { return calls.Load() != 1 }, 50*time.Millisecond, time.Millisecond)
	_, err = r.Logout(ctx, "test@example.com")
	require.NoError(t, err)
	select {
	case client := <-contexts:
		require.Equal(t, "124", client.UserID)
	case <-time.After(3 * time.Second):
		t.Fatal("logout did not refresh for the new anonymous account")
	}
	require.Eventually(t, func() bool {
		message, err := r.CurrentUserMessage()
		return err == nil && message != nil && message.AccountID == "124"
	}, 3*time.Second, time.Millisecond)
	service.SetActivity(false)
	a.ClearUser()
	message, err := r.CurrentUserMessage()
	require.NoError(t, err)
	require.Nil(t, message)
}

type switchingPeer struct {
	*fakePeerController
	onStart func()
}

func (p *switchingPeer) Start(ctx context.Context) error {
	p.onStart()
	return p.fakePeerController.Start(ctx)
}

func TestPatchSettingsAccountKeysWin(t *testing.T) {
	for _, token := range []string{"a", "new-a"} {
		t.Run(token, func(t *testing.T) {
			settings.Reset()
			t.Cleanup(settings.Reset)
			require.NoError(t, settings.InitSettings(t.TempDir()))
			require.NoError(t, settings.Patch(settings.Settings{settings.UserIDKey: int64(1), settings.TokenKey: "a"}))
			a := account.NewClient(http.DefaultClient, t.TempDir(), log.NoOpLogger())
			peer := &switchingPeer{fakePeerController: &fakePeerController{}, onStart: func() {
				require.NoError(t, a.PatchAccountSettings(settings.Settings{settings.UserIDKey: int64(2), settings.TokenKey: "b"}))
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &LocalBackend{ctx: ctx, peerClient: peer, accountClient: a}

			require.NoError(t, r.PatchSettings(settings.Settings{
				settings.PeerShareEnabledKey: true,
				settings.UserIDKey:           int64(1),
				settings.TokenKey:            token,
			}))

			require.Equal(t, int64(1), settings.GetInt64(settings.UserIDKey))
			require.Equal(t, token, settings.GetString(settings.TokenKey))
		})
	}
}
