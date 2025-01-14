package consumption

import (
	context "context"
	"testing"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gomock "go.uber.org/mock/gomock"
)

func TestNewStreamDialer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	var tests = []struct {
		name    string
		innerSD func() transport.StreamDialer
		assert  func(*testing.T, transport.StreamDialer, error)
	}{
		{
			name:    "it should fail when innerSD is nil",
			innerSD: func() transport.StreamDialer { return nil },
			assert: func(t *testing.T, sd transport.StreamDialer, err error) {
				assert.Error(t, err)
				assert.Nil(t, sd)
			},
		},
		{
			name: "it should return a StreamDialer when innerSD is not nil",
			innerSD: func() transport.StreamDialer {
				sd := NewMockStreamDialer(ctrl)
				return sd
			},
			assert: func(t *testing.T, sd transport.StreamDialer, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, sd)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sd, err := NewStreamDialer(tt.innerSD(), nil)
			tt.assert(t, sd, err)
		})
	}
}

func TestStreamDialer_DialStream(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	message := []byte("test")
	sd := NewMockStreamDialer(ctrl)
	streamConn := NewMockStreamConn(ctrl)
	rw := &rw{stream: streamConn}
	sd.EXPECT().DialStream(gomock.Any(), gomock.Any()).Return(streamConn, nil)
	streamConn.EXPECT().Write(gomock.Any()).Return(4, nil)
	streamConn.EXPECT().Read(gomock.Any()).DoAndReturn(func(p []byte) (int, error) {
		n := copy(p, message)
		return n, nil
	})

	dialer, err := NewStreamDialer(sd, nil)
	require.NoError(t, err)

	conn, err := dialer.DialStream(context.Background(), "")
	assert.NoError(t, err)
	assert.NotNil(t, conn)
	assert.Equal(t, conn, transport.WrapConn(streamConn, rw, rw))
	assert.Equal(t, uint64(0), DataSent.Load())
	assert.Equal(t, uint64(0), DataRecv.Load())

	n, err := conn.Write(message)
	assert.NoError(t, err)
	assert.Equal(t, len(message), n)

	assert.Equal(t, uint64(len(message)), DataSent.Load())
	assert.Equal(t, uint64(0), DataRecv.Load())

	response := make([]byte, 1024)
	n, err = conn.Read(response)
	assert.NoError(t, err)
	assert.Equal(t, len(message), n)

	assert.Equal(t, uint64(len(message)), DataSent.Load())
	assert.Equal(t, uint64(len(message)), DataRecv.Load())
}
