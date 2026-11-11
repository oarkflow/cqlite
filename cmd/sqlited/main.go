package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	db "github.com/oarkflow/sqlite"
	"github.com/oarkflow/sqlite/server/mysql"
)

func main() {
	path := flag.String("data", "./data", "database directory")
	addr := flag.String("addr", "127.0.0.1:3306", "listen address")
	flag.Parse()

	database, err := db.Open(*path, db.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()

	if err := (&mysql.Server{DB: database, Addr: *addr}).ListenAndServe(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
