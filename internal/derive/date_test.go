package derive

import (
	"testing"
	"time"
)

func TestSanityCheckedDate(t *testing.T) {
	internal := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		header   time.Time
		headerOK bool
		want     time.Time
	}{
		{
			name:     "header not present: uses internal",
			headerOK: false,
			want:     internal,
		},
		{
			name:     "header slightly behind internal (normal mail lag): used as-is",
			header:   internal.Add(-2 * time.Hour),
			headerOK: true,
			want:     internal.Add(-2 * time.Hour),
		},
		{
			name:     "header exactly at the future bound: used as-is",
			header:   internal.Add(maxFutureSkew),
			headerOK: true,
			want:     internal.Add(maxFutureSkew),
		},
		{
			name:     "header just past the future bound: falls back to internal",
			header:   internal.Add(maxFutureSkew + time.Second),
			headerOK: true,
			want:     internal,
		},
		{
			name:     "header exactly at the past bound: used as-is",
			header:   internal.Add(-maxPastSkew),
			headerOK: true,
			want:     internal.Add(-maxPastSkew),
		},
		{
			name:     "header just past the past bound: falls back to internal",
			header:   internal.Add(-maxPastSkew - time.Second),
			headerOK: true,
			want:     internal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanityCheckedDate(tt.header, tt.headerOK, internal)
			if !got.Equal(tt.want) {
				t.Errorf("sanityCheckedDate(%v, %v, %v) = %v, want %v", tt.header, tt.headerOK, internal, got, tt.want)
			}
		})
	}
}
