package carrier_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tholent/chaskiwasi/internal/carrier"
)

func TestFake_CallLogRecordsPayloadsInOrder(t *testing.T) {
	f := carrier.NewFake()
	ctx := context.Background()

	if err := f.Pututu(ctx, "CH1.1.aaa"); err != nil {
		t.Fatalf("Pututu: %v", err)
	}
	if err := f.Pututu(ctx, "CH1.2.bbb"); err != nil {
		t.Fatalf("Pututu: %v", err)
	}

	got := f.Sent()
	want := []string{"CH1.1.aaa", "CH1.2.bbb"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Sent() = %v, want %v", got, want)
	}
}

func TestFake_FailNext(t *testing.T) {
	f := carrier.NewFake()
	sentinel := errors.New("boom")
	f.FailNext(2, sentinel)

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := f.Pututu(ctx, "CH1.1.aaa"); !errors.Is(err, sentinel) {
			t.Fatalf("attempt %d: err = %v, want sentinel", i, err)
		}
	}
	if err := f.Pututu(ctx, "CH1.1.aaa"); err != nil {
		t.Fatalf("attempt after FailNext budget exhausted: %v, want nil", err)
	}

	calls := f.Calls()
	if len(calls) != 3 {
		t.Fatalf("Calls() has %d entries, want 3 (2 failures + 1 success)", len(calls))
	}
	if len(f.Sent()) != 1 {
		t.Fatalf("Sent() has %d entries, want 1", len(f.Sent()))
	}
}

func TestFake_BalanceDefaultsToUnsupported(t *testing.T) {
	f := carrier.NewFake()
	if _, err := f.Balance(context.Background()); !errors.Is(err, carrier.ErrUnsupported) {
		t.Fatalf("Balance() on a fresh Fake: err = %v, want ErrUnsupported", err)
	}

	f.SetBalance(carrier.Balance{Amount: 12.5, Currency: "USD"})
	bal, err := f.Balance(context.Background())
	if err != nil || bal.Amount != 12.5 || bal.Currency != "USD" {
		t.Fatalf("Balance() after SetBalance = %+v, %v", bal, err)
	}

	f.SetBalanceUnsupported()
	if _, err := f.Balance(context.Background()); !errors.Is(err, carrier.ErrUnsupported) {
		t.Fatalf("Balance() after SetBalanceUnsupported: err = %v, want ErrUnsupported", err)
	}
}
