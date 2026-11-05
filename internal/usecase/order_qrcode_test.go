package usecase

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/qrcode"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

type orderQRCodes struct {
	emptyQRCodes
	rows   []qrcode.QRCode
	failAt map[int64]error
}

func (r orderQRCodes) FindEnabledByAmount(_ context.Context, typ payment.Type, amount int64) (qrcode.QRCode, error) {
	if err := r.failAt[amount]; err != nil {
		return qrcode.QRCode{}, err
	}
	for _, row := range r.rows {
		if row.Type == typ && row.PriceCents == amount && row.State == qrcode.StateEnabled {
			return row, nil
		}
	}
	return qrcode.QRCode{}, port.ErrNotFound
}

func TestOrderSelectsEnabledFixedThenGeneralThenSettingsQRCode(t *testing.T) {
	for _, typ := range []payment.Type{payment.Wechat, payment.Alipay} {
		other := payment.Wechat
		key := setting.AlipayPayURLKey
		if typ == payment.Wechat {
			other = payment.Alipay
			key = setting.WechatPayURLKey
		}
		for _, scenario := range []struct {
			name     string
			rows     []qrcode.QRCode
			fallback string
			wantURL  string
			wantAuto bool
			wantCode ErrorCode
			failAt   map[int64]error
		}{
			{name: "fixed has priority", rows: []qrcode.QRCode{
				{Type: typ, PriceCents: 0, PayURL: "general"},
				{Type: typ, PriceCents: 1, PayURL: "fixed"},
			}, fallback: "settings", wantURL: "fixed"},
			{name: "uploaded general code works without settings", rows: []qrcode.QRCode{
				{Type: typ, PriceCents: 0, PayURL: "general"},
			}, wantURL: "general", wantAuto: true},
			{name: "general has priority over settings", rows: []qrcode.QRCode{
				{Type: typ, PriceCents: 0, PayURL: "general"},
			}, fallback: "settings", wantURL: "general", wantAuto: true},
			{name: "disabled and different channel codes are unavailable", rows: []qrcode.QRCode{
				{Type: typ, PriceCents: 0, PayURL: "disabled", State: qrcode.StateDisabled},
				{Type: other, PriceCents: 0, PayURL: "different-channel"},
			}, wantCode: CodeConfiguration},
			{name: "different fixed amount is unavailable", rows: []qrcode.QRCode{
				{Type: typ, PriceCents: 1000, PayURL: "ten-yuan-code"},
			}, wantCode: CodeConfiguration},
			{name: "settings fallback", fallback: "settings", wantURL: "settings", wantAuto: true},
			{name: "no available code", wantCode: CodeConfiguration},
			{name: "fixed lookup error is not hidden", fallback: "settings", failAt: map[int64]error{1: errors.New("database unavailable")}, wantCode: CodeDependency},
			{name: "general lookup error is not hidden", fallback: "settings", failAt: map[int64]error{0: errors.New("database unavailable")}, wantCode: CodeDependency},
		} {
			t.Run(fmt.Sprintf("%d/%s", typ, scenario.name), func(t *testing.T) {
				service := &OrderService{qrcodes: orderQRCodes{rows: scenario.rows, failAt: scenario.failAt}}
				url, auto, err := service.resolvePayURL(context.Background(), typ, 1, map[string]string{key: scenario.fallback})
				if scenario.wantCode != "" {
					if code, ok := ErrorCodeOf(err); !ok || code != scenario.wantCode {
						t.Fatalf("expected %s, got %v", scenario.wantCode, err)
					}
					return
				}
				if err != nil || url != scenario.wantURL || auto != scenario.wantAuto {
					t.Fatalf("got url=%q auto=%v err=%v; want url=%q auto=%v", url, auto, err, scenario.wantURL, scenario.wantAuto)
				}
			})
		}
	}
}
