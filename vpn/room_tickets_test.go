package vpn

import (
	"context"
	"slices"
	"testing"
	"time"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/helper"
)

func TestWithHelperRoomTicketsGivesTheConsumerInviterTickets(t *testing.T) {
	src := helper.NewSource(context.Background(), func(context.Context) (helper.Tickets, error) {
		return helper.Tickets{Inviter: []string{"i.1"}, Volunteer: []string{"v.1"}, Lifetime: 15 * time.Minute}, nil
	})
	get := lbA.RoomTicketSourceFromContext(withHelperRoomTickets(context.Background(), src))
	require.NotNil(t, get)
	require.Eventually(t, func() bool { return slices.Equal(get(), []string{"i.1"}) }, 2*time.Second, 10*time.Millisecond,
		"the consumer gets the inviter tickets, never the volunteer ones")
}

func TestWithHelperRoomTicketsWithoutASourceAddsNothing(t *testing.T) {
	assert.Nil(t, lbA.RoomTicketSourceFromContext(withHelperRoomTickets(context.Background(), nil)))
}

type countingInviter struct{ n int }

func (c *countingInviter) Inviter() []string { c.n++; return nil }

// The VPN client hands its ticket source to every tunnel it builds.
func TestVPNClientPassesItsInviterTicketsToTheTunnel(t *testing.T) {
	src := &countingInviter{}
	c := NewVPNClient(t.TempDir(), nil, nil)
	c.SetInviterTickets(src)
	tn := c.newTunnel(BoxOptions{})
	get := lbA.RoomTicketSourceFromContext(withHelperRoomTickets(context.Background(), tn.inviterTickets))
	require.NotNil(t, get)
	get()
	assert.Equal(t, 1, src.n)
}
