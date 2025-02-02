//     Copyright (C) 2020-2021, IrineSistiana
//
//     This file is part of mosdns.
//
//     mosdns is free software: you can redistribute it and/or modify
//     it under the terms of the GNU General Public License as published by
//     the Free Software Foundation, either version 3 of the License, or
//     (at your option) any later version.
//
//     mosdns is distributed in the hope that it will be useful,
//     but WITHOUT ANY WARRANTY; without even the implied warranty of
//     MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//     GNU General Public License for more details.
//
//     You should have received a copy of the GNU General Public License
//     along with this program.  If not, see <https://www.gnu.org/licenses/>.

package upstream

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/IrineSistiana/mosdns/v3/dispatcher/pkg/pool"
	"github.com/IrineSistiana/mosdns/v3/dispatcher/pkg/utils"
	"github.com/miekg/dns"
	"io"
	"net/http"
	"time"
)

const (
	defaultDoHTimeout = time.Second * 5
)

// DoH is a DNS-over-HTTPS (RFC 8484) upstream.
type DoH struct {
	// EndPoint is the DoH server URL.
	EndPoint string
	// Client is a http.Client that sends http requests.
	Client *http.Client
}

func (u *DoH) CloseIdleConnections() {
	u.Client.CloseIdleConnections()
}

var (
	allocator  = pool.NewAllocator(9) // 512 bytes
	bufPool512 = pool.NewBytesBufPool(512)
)

func (u *DoH) ExchangeContext(ctx context.Context, q []byte) ([]byte, error) {
	buf := allocator.Get(len(q))
	defer allocator.Release(buf)

	copy(buf, q)

	// In order to maximize HTTP cache friendliness, DoH clients using media
	// formats that include the ID field from the DNS message header, such
	// as "application/dns-message", SHOULD use a DNS ID of 0 in every DNS
	// request.
	// https://tools.ietf.org/html/rfc8484#section-4.1
	buf[0] = 0
	buf[1] = 0

	urlLen := len(u.EndPoint) + 5 + base64.RawURLEncoding.EncodedLen(len(buf))
	urlBuf := make([]byte, urlLen)

	// Padding characters for base64url MUST NOT be included.
	// See: https://tools.ietf.org/html/rfc8484#section-6.
	// That's why we use base64.RawURLEncoding.
	p := 0
	p += copy(urlBuf[p:], u.EndPoint)
	p += copy(urlBuf[p:], "?dns=")
	base64.RawURLEncoding.Encode(urlBuf[p:], buf)

	type result struct {
		r   []byte
		err error
	}

	resChan := make(chan *result, 1)
	go func() {
		// We overwrite the ctx with a fixed timout context here.
		// Because the http package may close the underlay connection
		// if the context is done before the query is completed. This
		// reduces the connection reuse rate.
		ctx, cancel := context.WithTimeout(context.Background(), defaultDoHTimeout)
		defer cancel()
		r, err := u.doHTTP(ctx, utils.BytesToStringUnsafe(urlBuf))
		resChan <- &result{r: r, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resChan:
		r := res.r
		err := res.err
		if err != nil {
			return nil, err
		}
		setMsgId(r, getMsgId(q))
		return r, nil
	}
}

func (u *DoH) doHTTP(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("interal err: NewRequestWithContext: %w", err)
	}

	req.Header["Accept"] = []string{"application/dns-message"}
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	// check status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad http status codes %d", resp.StatusCode)
	}

	bb := bufPool512.Get()
	defer bufPool512.Release(bb)
	_, err = bb.ReadFrom(io.LimitReader(resp.Body, dns.MaxMsgSize))
	if err != nil {
		return nil, fmt.Errorf("failed to read http body: %w", err)
	}

	r := make([]byte, bb.Len())
	copy(r, bb.Bytes())
	return r, nil
}
