package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/metal-toolbox/cookieflipper/app"
	"github.com/metal-toolbox/cookieflipper/pkg/types"
)

type Serverservice struct{}

func (s *Serverservice) CookieByName(ctx context.Context, name string) (*types.Cookie, error) {
	return &types.Cookie{ID: uuid.New()}, nil
}

func New(config *app.ServerserviceOptions) Repository {
	return &Serverservice{}
}
