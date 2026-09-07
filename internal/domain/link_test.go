package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeTargetURL(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in      string
		want    string
		wantErr bool
	}{
		"plain https":          {in: "https://example.com", want: "https://example.com"},
		"keeps path and query": {in: "https://example.com/a/b?x=1&y=2", want: "https://example.com/a/b?x=1&y=2"},
		"trims whitespace":     {in: "  https://example.com/x  ", want: "https://example.com/x"},
		"http allowed":         {in: "http://example.com", want: "http://example.com"},
		"public ip allowed":    {in: "https://8.8.8.8/x", want: "https://8.8.8.8/x"},

		"empty":            {in: "", wantErr: true},
		"whitespace only":  {in: "   ", wantErr: true},
		"no scheme":        {in: "example.com", wantErr: true},
		"ftp scheme":       {in: "ftp://example.com", wantErr: true},
		"javascript":       {in: "javascript:alert(1)", wantErr: true},
		"data uri":         {in: "data:text/html;base64,PHNjcmlwdD4=", wantErr: true},
		"file scheme":      {in: "file:///etc/passwd", wantErr: true},
		"no host":          {in: "https://", wantErr: true},
		"loopback ip":      {in: "http://127.0.0.1:8080/admin", wantErr: true},
		"loopback name":    {in: "http://localhost:8080/admin", wantErr: true},
		"private 10/8":     {in: "http://10.0.0.5/", wantErr: true},
		"private 192.168":  {in: "http://192.168.1.1/", wantErr: true},
		"link local meta":  {in: "http://169.254.169.254/latest/meta-data/", wantErr: true},
		"cgnat":            {in: "http://100.64.0.1/", wantErr: true},
		"ipv6 loopback":    {in: "http://[::1]/", wantErr: true},
		"mdns local":       {in: "http://printer.local/", wantErr: true},
		"unspecified addr": {in: "http://0.0.0.0/", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeTargetURL(tc.in)
			if tc.wantErr {
				require.Error(t, err)

				var ve *ValidationError
				require.True(t, errors.As(err, &ve), "expected a *ValidationError, got %T", err)
				assert.Equal(t, "target_url", ve.Field)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNormalizeTargetURL_RejectsOverlyLongInput(t *testing.T) {
	t.Parallel()

	long := "https://example.com/?q=" + string(make([]byte, maxTargetURLLength))
	_, err := NormalizeTargetURL(long)
	assert.Error(t, err)
}

func TestValidateAlias(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		alias   string
		wantErr bool
	}{
		"simple":            {alias: "my-link"},
		"underscores":       {alias: "my_link_2"},
		"mixed case":        {alias: "MyLink"},
		"minimum length":    {alias: "abc"},
		"too short":         {alias: "ab", wantErr: true},
		"too long":          {alias: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wantErr: true},
		"empty":             {alias: "", wantErr: true},
		"slash":             {alias: "a/b", wantErr: true},
		"dot":               {alias: "a.b", wantErr: true},
		"space":             {alias: "a b", wantErr: true},
		"unicode":           {alias: "посилання", wantErr: true},
		"reserved api":      {alias: "api", wantErr: true},
		"reserved any case": {alias: "MeTrIcS", wantErr: true},
		"reserved healthz":  {alias: "healthz", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := ValidateAlias(tc.alias)
			if tc.wantErr {
				var ve *ValidationError
				require.Error(t, err)
				require.True(t, errors.As(err, &ve))
				assert.Equal(t, "alias", ve.Field)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestLink_IsExpired(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	assert.False(t, (&Link{}).IsExpired(now), "a link without an expiry never expires")
	assert.False(t, (&Link{ExpiresAt: &future}).IsExpired(now))
	assert.True(t, (&Link{ExpiresAt: &past}).IsExpired(now))
	assert.True(t, (&Link{ExpiresAt: &now}).IsExpired(now), "expiry is inclusive")
}
