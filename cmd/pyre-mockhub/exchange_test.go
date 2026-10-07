package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
)

func exchange(ctx context.Context, url, pin string, ex c.PairingExchange) (c.ClientCredential, error) {
	tc, err := client.PinnedTLSConfig(pin)
	if err != nil {
		return c.ClientCredential{}, err
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}
	body, _ := json.Marshal(ex)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+c.PathPairExchange, bytes.NewReader(body))
	resp, err := hc.Do(req)
	if err != nil {
		return c.ClientCredential{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var eb c.ErrorBody
		if json.Unmarshal(b, &eb) == nil && eb.Error != nil {
			return c.ClientCredential{}, eb.Error
		}
		return c.ClientCredential{}, c.Errorf(c.ErrInternal, "status %d", resp.StatusCode)
	}
	var cred c.ClientCredential
	return cred, json.Unmarshal(b, &cred)
}
