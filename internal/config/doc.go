// Package config parses the server's environment.
//
// Every setting is an OOA_* variable; the struct tags in config.go are the
// source of truth for names and defaults. Load validates after parsing, so
// helpers like PortRange and AbstractorURL can trust their input.
//
// Some values (the scheduler strategy) only seed a runtime setting stored in
// the database; the code that reads both decides which one wins.
package config
