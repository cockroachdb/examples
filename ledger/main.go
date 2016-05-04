// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Tobias Schottdorf (tobias.schottdorf@gmail.com)

// This example simulates a (particular) banking ledger. Depending on the
// chosen generator and concurrency, the workload carried out is contended
// or entirely non-overlapping.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"time"

	// Import postgres driver.
	"github.com/cockroachdb/cockroach-go/crdb"
	"github.com/cockroachdb/pq"
)

const stmtCreate = `
CREATE TABLE accounts (
  causality_id BIGINT NOT NULL,
  posting_group_id BIGINT NOT NULL,

  amount BIGINT,
  balance BIGINT,
  currency VARCHAR,

  created TIMESTAMP,
  value_date TIMESTAMP,

  account_id VARCHAR,
  transaction_id VARCHAR,

  scheme VARCHAR,

  PRIMARY KEY (account_id, causality_id),
  UNIQUE (account_id, posting_group_id)
);`

var concurrency = flag.Int("concurrency", 5, "Number of concurrent actors moving money.")
var generator = flag.String("generator", "few-few", "Type of action. One of few-few, many-many or few-one.")
var noRunningBalance = flag.Bool("no-running-balance", false, "Do not keep a running balance per account. Avoids contention.")

func init() {
	rand.Seed(time.Now().UnixNano())
}

var usage = func() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s <db URL>\n\n", os.Args[0])
	flag.PrintDefaults()
}

type postingRequest struct {
	Group              int64
	AccountA, AccountB string
	Amount             int64 // deposited on AccountA, removed from AccountB
	Currency           string

	Transaction, Scheme string // opaque
}

var goldenReq = postingRequest{
	Group:    1,
	AccountA: "myacc",
	AccountB: "youracc",
	Amount:   5,
	Currency: "USD",
}

type genFn func() postingRequest

var generators = map[string]genFn{
	// Uncontended.
	"many-many": func() postingRequest {
		req := goldenReq
		req.AccountA = fmt.Sprintf("acc%d", rand.Int63())
		req.AccountB = fmt.Sprintf("acc%d", rand.Int63())
		req.Group = rand.Int63()
		return req
	},
	// Mildly contended: 10 users shuffling money around among each other.
	"few-few": func() postingRequest {
		req := goldenReq
		req.AccountA = fmt.Sprintf("acc%d", rand.Intn(10))
		req.AccountB = fmt.Sprintf("acc%d", rand.Intn(10))
		req.Group = rand.Int63()
		if req.Group%100 == 0 {
			// Create some fake contention in ~1% of the requests.
			req.Group = 1
		}
		return req
	},
	// Highly contended: 10 users all involving one peer account.
	"few-one": func() postingRequest {
		req := goldenReq
		req.AccountA = fmt.Sprintf("acc%d", rand.Intn(10))
		req.AccountB = "outbound_wash"
		req.Group = rand.Int63()
		return req
	},
}

func getLast(tx *sql.Tx, accountID string) (lastCID int64, lastBalance int64, err error) {
	err = tx.QueryRow(`SELECT causality_id, balance FROM accounts `+
		`WHERE account_id = $1 ORDER BY causality_id DESC LIMIT 1`, accountID).
		Scan(&lastCID, &lastBalance)

	if err == sql.ErrNoRows {
		err = nil
		// Paranoia about unspecified semantics.
		lastBalance = 0
		lastCID = 0
	}
	return
}

func doPosting(tx *sql.Tx, req postingRequest) error {
	var cidA, balA, cidB, balB int64
	if *noRunningBalance {
		var err error
		cidA, balA, err = getLast(tx, req.AccountA)
		if err != nil {
			return err
		}
		cidB, balB, err = getLast(tx, req.AccountB)
		if err != nil {
			return err
		}
	} else {
		// For Cockroach, unique_rowid() would be the better choice.
		cidA, cidB = rand.Int63(), rand.Int63()
		// Want the running balance to always be zero in this case without
		// special-casing below.
		balA = -req.Amount
		balB = req.Amount
	}
	_, err := tx.Exec(`
INSERT INTO accounts (
  posting_group_id,
  amount,
  account_id,
  causality_id, -- strictly increasing in absolute time. Only used for running balance.
  balance
)
VALUES (
  $1,	-- posting_group_id
  $2, 	-- amount
  $3, 	-- account_id (A)
  $4, 	-- causality_id
  $5+CAST($2 AS BIGINT) -- (new) balance (Postgres needs the cast)
), (
  $1,   -- posting_group_id
 -$2,   -- amount
  $6,   -- account_id (B)
  $7,   -- causality_id
  $8-$2 -- (new) balance
)`, req.Group, req.Amount,
		req.AccountA, cidA+1, balA,
		req.AccountB, cidB+1, balB)
	return err
}

func worker(db *sql.DB, l func(string, ...interface{}), gen func() postingRequest) {
	for {
		req := gen()
		l("running %v", req)
		if err := crdb.ExecuteTx(db, func(tx *sql.Tx) error {
			return doPosting(tx, req)
		}); err != nil {
			pqErr, ok := err.(*pq.Error)
			if ok {
				if pqErr.Code.Class() == pq.ErrorClass("23") {
					// Integrity violations. Note that (especially with Postgres)
					// the primary key will often be violated under congestion.
					l("%s", pqErr)
					continue
				}
				if pqErr.Code.Class() == pq.ErrorClass("40") {
					// Transaction rollback errors (e.g. Postgres
					// serializability restarts)
					l("%s", pqErr)
					continue
				}
			}
			log.Fatal(err)
		} else {
			l("success")
		}
	}
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		os.Exit(2)
	}

	gen, ok := generators[*generator]
	if !ok {
		usage()
		os.Exit(2)
	}

	dbURL := flag.Arg(0)

	parsedURL, err := url.Parse(dbURL)
	if err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("postgres", parsedURL.String())
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// Ignoring the error is the easiest way to be reasonably sure the db+table
	// exist without bloating the example.
	_, _ = db.Exec(`CREATE DATABASE ledger`)
	_, _ = db.Exec(stmtCreate)

	db.SetMaxOpenConns(*concurrency)

	for i := 0; i < *concurrency; i++ {
		num := i
		go worker(db, func(s string, args ...interface{}) {
			log.Printf(strconv.Itoa(num)+": "+s, args...)
		}, gen)
	}

	<-chan struct{}(nil)
}
