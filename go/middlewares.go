package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

func appAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		c, err := r.Cookie("app_session")
		if errors.Is(err, http.ErrNoCookie) || c.Value == "" {
			writeError(w, http.StatusUnauthorized, errors.New("app_session cookie is required"))
			return
		}
		accessToken := c.Value
		user, ok := userCache.get(accessToken)
		if !ok {
			user = &User{}
			if err := db.GetContext(ctx, user, "SELECT * FROM users WHERE access_token = ?", accessToken); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusUnauthorized, errors.New("invalid access token"))
					return
				}
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			userCache.set(accessToken, user)
		}

		ctx = context.WithValue(ctx, "user", user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func ownerAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		c, err := r.Cookie("owner_session")
		if errors.Is(err, http.ErrNoCookie) || c.Value == "" {
			writeError(w, http.StatusUnauthorized, errors.New("owner_session cookie is required"))
			return
		}
		accessToken := c.Value
		owner, ok := ownerCache.get(accessToken)
		if !ok {
			owner = &Owner{}
			if err := db.GetContext(ctx, owner, "SELECT * FROM owners WHERE access_token = ?", accessToken); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusUnauthorized, errors.New("invalid access token"))
					return
				}
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			ownerCache.set(accessToken, owner)
		}

		ctx = context.WithValue(ctx, "owner", owner)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func chairAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		c, err := r.Cookie("chair_session")
		if errors.Is(err, http.ErrNoCookie) || c.Value == "" {
			writeError(w, http.StatusUnauthorized, errors.New("chair_session cookie is required"))
			return
		}
		accessToken := c.Value
		chair, ok := chairCache.get(accessToken)
		if !ok {
			chair = &chairIdentity{}
			if err := db.GetContext(ctx, chair, "SELECT id, owner_id, name, model FROM chairs WHERE access_token = ?", accessToken); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusUnauthorized, errors.New("invalid access token"))
					return
				}
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			chairCache.set(accessToken, chair)
		}

		ctx = context.WithValue(ctx, "chair", chair)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
