package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	db "github.com/sujit/sqlite"
)

func main() {
	path := flag.String("data", "./data", "database directory")
	flag.Parse()

	database, err := db.Open(*path, db.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("sqlite> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("sqlite> ")
			continue
		}
		if line == ".quit" || line == ".exit" {
			return
		}
		if strings.HasPrefix(strings.ToUpper(line), "SELECT") || strings.HasPrefix(strings.ToUpper(line), "EXPLAIN") {
			rows, err := database.Query(context.Background(), line)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				fmt.Print("sqlite> ")
				continue
			}
			fmt.Println(strings.Join(rows.Columns, "\t"))
			for {
				row, ok := rows.Next()
				if !ok {
					break
				}
				for i, v := range row {
					if i > 0 {
						fmt.Print("\t")
					}
					fmt.Print(v)
				}
				fmt.Println()
			}
		} else {
			res, err := database.Exec(context.Background(), line)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			} else {
				fmt.Printf("ok rows=%d last_insert_id=%d\n", res.RowsAffected, res.LastInsertID)
			}
		}
		fmt.Print("sqlite> ")
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
