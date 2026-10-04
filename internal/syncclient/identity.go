package syncclient

import (
	"context"
	"net/http"

	"agentbox/internal/syncproto"
)

// Identity requires the authenticated new identity contract. Legacy capability
// fallback remains in the desktop terminal path, never in automatic sync.
func (r *Remote) Identity(ctx context.Context) (syncproto.ServerIdentity, error) {
	var identity syncproto.ServerIdentity
	address := *r.base
	address.Path += "api/clients/capabilities"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address.String(), nil)
	if err != nil {
		return identity, syncproto.ErrInvalid
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	response, err := r.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return identity, ctx.Err()
		}
		return identity, transportFailure(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return identity, &HTTPError{response.StatusCode}
	}
	if err = decodeRemote(response, 16<<10, &identity); err != nil {
		return identity, err
	}
	if identity.Validate() != nil {
		return syncproto.ServerIdentity{}, ErrProtocol
	}
	return identity, nil
}

// ForBinding returns an immutable pinned transport after checking both server
// and authenticated user. It does not enable sync or authorize any plan.
func (r *Remote) ForBinding(ctx context.Context, binding syncproto.Binding) (*Remote, error) {
	if binding.Validate() != nil || binding.Server != r.base.String() {
		return nil, ErrBinding
	}
	identity, err := r.Identity(ctx)
	if err != nil {
		return nil, err
	}
	if identity.ServerID != binding.ServerID || identity.User != binding.User {
		return nil, ErrBinding
	}
	pinned := *r
	pinned.serverID = binding.ServerID
	return &pinned, nil
}
