package ui

import (
	"context"
	"net/url"
)

// This file adds the control-plane client methods the admin UI's Endpoints page
// (Batch 29) needs: list, create, update, and delete model endpoints. A model
// endpoint registers an upstream model provider and stores its real API key
// sealed server-side — the key is write-only and never returned, so the schemas
// here carry only SecretSet (whether a key is on file), never the key itself.
// The calls follow the same shape as client.go: send the caller's session token
// as a Bearer credential and decode the typed success body or the flat Error
// envelope. The REST resource is /model-endpoints; the admin UI mounts its pages
// at /endpoints.

// endpointsPath is the model-endpoints REST collection.
const endpointsPath = "/model-endpoints"

// Endpoint mirrors the ModelEndpoint schema: a registered upstream model
// endpoint. SecretSet reports whether a sealed API key is on file (the key is
// write-only and never returned); Active gates whether the endpoint may be used
// to launch sessions.
type Endpoint struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id,omitempty"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	Active    bool   `json:"active"`
	SecretSet bool   `json:"secret_set"`
	CreatedAt string `json:"created_at"`
}

// EndpointList mirrors the ModelEndpointList schema — a plain {items} envelope
// (an org's endpoints are a small, bounded set returned whole).
type EndpointList struct {
	Items []Endpoint `json:"items"`
}

// EndpointCreate is the POST /model-endpoints body. APIKey is the real upstream
// credential; it is sealed server-side and never returned. An empty APIKey
// registers the endpoint with no key (secret_set:false). A key supplied when the
// server has no vault key configured is a 400 whose message the caller surfaces.
type EndpointCreate struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key,omitempty"`
}

// EndpointUpdate is the PATCH /model-endpoints/{id} body. Every field is a
// pointer so an omitted field is left unchanged; a non-nil APIKey re-seals the
// stored key (a blank one is left untouched by omitting the field entirely).
type EndpointUpdate struct {
	Name     *string `json:"name,omitempty"`
	Provider *string `json:"provider,omitempty"`
	BaseURL  *string `json:"base_url,omitempty"`
	APIKey   *string `json:"api_key,omitempty"`
	Active   *bool   `json:"active,omitempty"`
}

// ListEndpoints calls GET /model-endpoints and returns the org's model
// endpoints. Requires endpoints.manage. The API key is never included.
func (c *Client) ListEndpoints(ctx context.Context, token string) (*EndpointList, error) {
	var list EndpointList
	if err := c.getJSON(ctx, token, endpointsPath, nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// CreateEndpoint calls POST /model-endpoints to register a model endpoint and
// returns it. A duplicate name is a 409; a supplied key with no server-side
// vault key configured is a 400 (surface its message). Requires endpoints.manage.
func (c *Client) CreateEndpoint(ctx context.Context, token string, req EndpointCreate) (*Endpoint, error) {
	var endpoint Endpoint
	if err := c.postJSON(ctx, token, endpointsPath, req, &endpoint); err != nil {
		return nil, err
	}
	return &endpoint, nil
}

// UpdateEndpoint calls PATCH /model-endpoints/{id} with the supplied partial
// update and returns the updated endpoint. Requires endpoints.manage.
func (c *Client) UpdateEndpoint(ctx context.Context, token, id string, req EndpointUpdate) (*Endpoint, error) {
	var endpoint Endpoint
	if err := c.patchJSON(ctx, token, endpointsPath+"/"+url.PathEscape(id), req, &endpoint); err != nil {
		return nil, err
	}
	return &endpoint, nil
}

// DeleteEndpoint calls DELETE /model-endpoints/{id} (a 204 on success). Requires
// endpoints.manage.
func (c *Client) DeleteEndpoint(ctx context.Context, token, id string) error {
	return c.deleteJSON(ctx, token, endpointsPath+"/"+url.PathEscape(id))
}
