package store

import (
	"context"

	"github.com/metal-toolbox/cookieflipper/pkg/types"
)

type Repository interface {
	CookieByName(ctx context.Context, name string) (*types.Cookie, error)
}
