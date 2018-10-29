/*
 * Copyright 2018 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package client

import (
	"net/url"
	"strings"
)

// Config is a configuration parsed from a DSN string.
type Config struct {
	DatabaseID string

	// additional configs should be filled
	// such as read/write/exec timeout
	// currently no timeout is supported.
}

// NewConfig creates a new config with default value.
func NewConfig() *Config {
	return &Config{}
}

// FormatDSN formats the given Config into a DSN string which can be passed to the driver.
func (cfg *Config) FormatDSN() string {
	// build query from arguments

	u := &url.URL{
		Scheme: DBScheme,
		Host:   cfg.DatabaseID,
	}

	newQuery := u.Query()
	u.RawQuery = newQuery.Encode()

	return u.String()
}

// ParseDSN parse the DSN string to a Config.
func ParseDSN(dsn string) (cfg *Config, err error) {
	if !strings.HasPrefix(dsn, DBScheme) && !strings.HasPrefix(dsn, DBSchemeAlias) {
		dsn = DBScheme + "://" + dsn
	}

	var u *url.URL
	if u, err = url.Parse(dsn); err != nil {
		return
	}

	cfg = NewConfig()
	cfg.DatabaseID = u.Host

	return
}
