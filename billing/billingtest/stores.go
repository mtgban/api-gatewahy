package billingtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/mtgban/api-gatewahy/billing"
)

// FakeStores is a billing.StoreLister over two fixed game sites.
type FakeStores struct {
	Sites map[string]billing.SiteStores
	// Fail makes every call error; Down makes one game's calls error.
	Fail error
	Down map[string]bool
	// Calls counts lookups per game, failed ones included.
	Calls map[string]int

	mu sync.Mutex
}

var _ billing.StoreLister = (*FakeStores)(nil)

// NewFakeStores lists magic and pokemon, both implying TCGplayer.
func NewFakeStores() *FakeStores {
	tcg := billing.StoreFamily{Key: "tcgplayer", Name: "TCGplayer", Shorthands: []string{"TCGLow", "TCGMarket", "TCGDirect", "TCGDirectNet", "TCGPlayer"}}
	return &FakeStores{Calls: map[string]int{}, Sites: map[string]billing.SiteStores{
		"magic": {Game: "magic", Implied: []billing.StoreFamily{tcg}, Stores: []billing.StoreFamily{
			{Key: "cardkingdom", Name: "Card Kingdom", Shorthands: []string{"CK"}},
			{Key: "coolstuffinc", Name: "CoolStuffInc", Shorthands: []string{"CSI"}},
			{Key: "starcitygames", Name: "Star City Games", Shorthands: []string{"SCG"}},
		}},
		"pokemon": {Game: "pokemon", Implied: []billing.StoreFamily{tcg}, Stores: []billing.StoreFamily{
			{Key: "cardkingdom", Name: "Card Kingdom", Shorthands: []string{"CK", "CKBLLast"}},
			{Key: "trollandtoad", Name: "Troll and Toad", Shorthands: []string{"TNT"}},
		}},
	}}
}

// SiteStores implements billing.StoreLister.
func (f *FakeStores) SiteStores(_ context.Context, game string) (billing.SiteStores, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Calls == nil {
		f.Calls = map[string]int{}
	}
	f.Calls[game]++
	if f.Fail != nil {
		return billing.SiteStores{}, f.Fail
	}
	if f.Down[game] {
		return billing.SiteStores{}, fmt.Errorf("fake stores: %s is down", game)
	}
	site, ok := f.Sites[game]
	if !ok {
		return billing.SiteStores{}, fmt.Errorf("fake stores: no site for %s", game)
	}
	return site, nil
}

// TotalCalls is the number of lookups across every game.
func (f *FakeStores) TotalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		n += c
	}
	return n
}
