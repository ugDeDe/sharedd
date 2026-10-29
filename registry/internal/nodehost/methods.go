package nodehost

// Typed API methods on Client (docs/specs/hosting-blueprints/02-provider-api.md §2).
// All input validation happens before I/O and returns failure("invalid_input").
// Path IDs go through validID + url.PathEscape; query values through url.Values.

import (
	"context"
	"net/url"
	"strconv"
)

const (
	maxName   = 100 // name bound per provider docs
	optBound  = 64  // enum-ish option strings (location, period, speed, diskType, os, cpu, promo)
	idemBound = 128 // Idempotency-Key length bound
)

var periodValues = map[string]bool{"1d": true, "3d": true, "1w": true, "1m": true, "3m": true, "6m": true, "1y": true}

func idPath(id string) (string, error) {
	if !validID(id) {
		return "", failure("invalid_input")
	}
	return url.PathEscape(id), nil
}

func optText(s string) bool { return s == "" || text(s, optBound) }

func (r EstimateRequest) validate() bool {
	return r.Cores > 0 && r.RAM > 0 && r.Disk > 0 &&
		optText(r.NetworkSpeed) && optText(r.Location) &&
		(r.Period == "" || periodValues[r.Period])
}

func (r CreateRequest) validate() bool {
	return text(r.Name, maxName) && text(r.CPU, optBound) &&
		r.Cores > 0 && r.RAM > 0 && r.Disk > 0 &&
		text(r.DiskType, optBound) && text(r.OS, optBound) && text(r.Location, optBound) &&
		optText(r.NetworkSpeed) && optText(r.PromoCode) &&
		(r.Period == "" || periodValues[r.Period])
}

// Account returns the API account card including the balance snapshot source.
func (c *Client) Account(ctx context.Context) (*Account, error) {
	var out Account
	if err := c.do(ctx, "GET", "/v1/account", "", nil, &out, 200); err != nil {
		return nil, err
	}
	return &out, nil
}

// AccountBalance maps /v1/account to the normalized balance DTO. USD and
// scope nodehost_api per provider docs; amount is an exact decimal string.
func (c *Client) AccountBalance(ctx context.Context) (*Balance, error) {
	a, err := c.Account(ctx)
	if err != nil {
		return nil, err
	}
	return &Balance{Amount: a.Balance, Currency: "USD", Scope: "nodehost_api"}, nil
}

func (c *Client) Locations(ctx context.Context) ([]Location, error) {
	var out []Location
	if err := c.do(ctx, "GET", "/v1/catalog/locations", "", nil, &out, 200); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) OSTemplates(ctx context.Context) ([]OSTemplate, error) {
	var out []OSTemplate
	if err := c.do(ctx, "GET", "/v1/catalog/os-templates", "", nil, &out, 200); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) CPUModels(ctx context.Context, location string) ([]string, error) {
	if !optText(location) {
		return nil, failure("invalid_input")
	}
	path := "/v1/catalog/cpu-models"
	if location != "" {
		path += "?location=" + url.QueryEscape(location)
	}
	var out []string
	if err := c.do(ctx, "GET", path, "", nil, &out, 200); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) DiskTypes(ctx context.Context) ([]DiskType, error) {
	var out []DiskType
	if err := c.do(ctx, "GET", "/v1/catalog/disk-types", "", nil, &out, 200); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) NetworkOptions(ctx context.Context, location string) (*NetworkOptions, error) {
	if !text(location, optBound) {
		return nil, failure("invalid_input")
	}
	var out NetworkOptions
	path := "/v1/catalog/network-options?location=" + url.QueryEscape(location)
	if err := c.do(ctx, "GET", path, "", nil, &out, 200); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Limits(ctx context.Context) (*Limits, error) {
	var out Limits
	if err := c.do(ctx, "GET", "/v1/catalog/limits", "", nil, &out, 200); err != nil {
		return nil, err
	}
	return &out, nil
}

// Estimate prices a configuration without any purchase side effect.
// Live success is 201 (2026-10-17); 200 is also accepted.
func (c *Client) Estimate(ctx context.Context, req EstimateRequest) (*Estimate, error) {
	if !req.validate() {
		return nil, failure("invalid_input")
	}
	var out Estimate
	if err := c.do(ctx, "POST", "/v1/pricing/estimate", "", req, &out, 200, 201); err != nil {
		return nil, err
	}
	return &out, nil
}

// Create orders a server. idemKey must be fixed by the caller before the first
// I/O and never changed across retries; 202 returns an async operation handle.
// A mutation result without operationId is not usable for reconciliation.
func (c *Client) Create(ctx context.Context, req CreateRequest, idemKey string) (*CreateResult, error) {
	if !req.validate() || !text(idemKey, idemBound) {
		return nil, failure("invalid_input")
	}
	var out CreateResult
	if err := c.do(ctx, "POST", "/v1/servers", idemKey, req, &out, 202); err != nil {
		return nil, err
	}
	if out.OperationID == "" {
		return nil, failure("invalid_response")
	}
	return &out, nil
}

// Operation returns the operation handle. Unlike other reads, a *failed*
// operation arrives as a 200 body with status:"failed" and an error.code —
// it is returned as a value (Operation.Status/Operation.Error.Code), not
// collapsed into provider_error, so the polling layer can distinguish
// e.g. insufficient funds. Strings/objects map to local allowlisted codes; the provider
// message text is never carried.
func (c *Client) Operation(ctx context.Context, id string) (*Operation, error) {
	p, err := idPath(id)
	if err != nil {
		return nil, err
	}
	var out Operation
	if err := c.doDecode(ctx, "GET", "/v1/operations/"+p, "", nil, &out, []int{200}, false); err != nil {
		return nil, err
	}
	switch out.Status {
	case "pending", "queued", "running", "processing", "failed":
	case "succeeded":
		if !validID(out.ResourceID) {
			return nil, failure("invalid_response")
		}
	default:
		return nil, failure("invalid_response")
	}
	return &out, nil
}

// ServersPage reads one page; the caller loops on NextCursor and must guard
// against a provider returning the same cursor repeatedly.
func (c *Client) ServersPage(ctx context.Context, cursor string, limit int) (*ServerPage, error) {
	if limit < 1 || limit > 100 || !optText(cursor) {
		return nil, failure("invalid_input")
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out ServerPage
	if err := c.do(ctx, "GET", "/v1/servers?"+q.Encode(), "", nil, &out, 200); err != nil {
		return nil, err
	}
	return &out, nil
}

// Server returns the full card including the secret password, captured only
// into the struct field (json:"-" on the wire side of any re-marshal).
func (c *Client) Server(ctx context.Context, id string) (*Server, error) {
	p, err := idPath(id)
	if err != nil {
		return nil, err
	}
	var out Server
	if err := c.do(ctx, "GET", "/v1/servers/"+p, "", nil, &out, 200); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete result is not proof that billing is already released; confirm by read.
func (c *Client) Delete(ctx context.Context, id string) (*DeleteResult, error) {
	p, err := idPath(id)
	if err != nil {
		return nil, err
	}
	var out DeleteResult
	if err := c.do(ctx, "DELETE", "/v1/servers/"+p, "", nil, &out, 200); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) IPSubnets(ctx context.Context, id string) ([]Subnet, error) {
	p, err := idPath(id)
	if err != nil {
		return nil, err
	}
	var out []Subnet
	if err := c.do(ctx, "GET", "/v1/servers/"+p+"/ip-subnets", "", nil, &out, 200); err != nil {
		return nil, err
	}
	return out, nil
}

// ChangeIP has no documented idempotency: a lost response must never be
// retried by re-issuing this POST. useSubnet selects {subnetId:N} vs {} body.
func (c *Client) ChangeIP(ctx context.Context, id string, subnetID int, useSubnet bool) (*ChangeIPResult, error) {
	p, err := idPath(id)
	if err != nil {
		return nil, err
	}
	var body any = struct{}{}
	if useSubnet {
		if subnetID <= 0 {
			return nil, failure("invalid_input")
		}
		body = struct {
			SubnetID int `json:"subnetId"`
		}{subnetID}
	}
	var out ChangeIPResult
	if err := c.do(ctx, "POST", "/v1/servers/"+p+"/change-ip", "", body, &out, 200); err != nil {
		return nil, err
	}
	return &out, nil
}
