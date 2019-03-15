package accesslist

import (
	"testing"

	"github.com/semihalev/log"
	"github.com/faceair/go-dnsmasq/config"
	"github.com/faceair/go-dnsmasq/ctx"
	"github.com/faceair/go-dnsmasq/middleware"
	"github.com/faceair/go-dnsmasq/mock"
	"github.com/stretchr/testify/assert"
)

func Test_Accesslist(t *testing.T) {
	log.Root().SetHandler(log.LvlFilterHandler(0, log.StdoutHandler))

	cfg := new(config.Config)
	cfg.AccessList = []string{"127.0.0.1/32", "1"}

	middleware.Setup(cfg)

	a := middleware.Get("accesslist").(*AccessList)
	assert.Equal(t, "accesslist", a.Name())

	dc := ctx.New([]ctx.Handler{})

	mw := mock.NewWriter("udp", "127.0.0.1:0")
	dc.DNSWriter = mw
	a.ServeDNS(dc)

	mw = mock.NewWriter("udp", "0.0.0.0:0")
	dc.DNSWriter = mw
	a.ServeDNS(dc)
}
