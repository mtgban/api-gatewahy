package apiaccess

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SQLConfig is apiaccess's own DB config; its JSON keys match timeseries.SQLConfig.
type SQLConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
	SSLMode  string `json:"sslmode"`
	// ReadOnly is accepted so a shared config parses, and ignored: apiaccess always writes.
	ReadOnly               bool `json:"readonly"`
	MaxOpenConns           int  `json:"max_open_conns"`
	MaxIdleConns           int  `json:"max_idle_conns"`
	ConnMaxLifetimeSeconds int  `json:"conn_max_lifetime_seconds"`
}

// quoteSQLValue wraps s in single quotes for a lib/pq key=value DSN,
// backslash-escaping the characters that would otherwise end the value early.
func quoteSQLValue(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\\' || r == '\'' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// DSN renders the config as a lib/pq connection string, quoting every value
// so an empty password or one containing a space or quote still parses.
func (c SQLConfig) DSN() string {
	sslMode := c.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		quoteSQLValue(c.Host), c.Port, quoteSQLValue(c.User), quoteSQLValue(c.Password),
		quoteSQLValue(c.DBName), quoteSQLValue(sslMode),
	)
}

// OpenDB opens a connection pool for the config, applying its pool limits.
func (c SQLConfig) OpenDB() (*sql.DB, error) {
	db, err := sql.Open("postgres", c.DSN())
	if err != nil {
		return nil, err
	}

	maxOpen := c.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 25
	}
	maxIdle := c.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = maxOpen
	}
	lifetime := time.Duration(c.ConnMaxLifetimeSeconds) * time.Second
	if lifetime <= 0 {
		lifetime = 30 * time.Minute
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(lifetime)

	return db, nil
}
