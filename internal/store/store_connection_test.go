package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestEveryPooledSQLiteConnectionKeepsIntegrityPragmas(t *testing.T) {
	ctx := context.Background()
	ledger, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	connections := make([]*sql.Conn, 0, 8)
	for index := 0; index < 8; index++ {
		connection, err := ledger.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
	}
	defer func() {
		for _, connection := range connections {
			connection.Close()
		}
	}()
	for index, connection := range connections {
		var foreignKeys, synchronous, busyTimeout int
		if err := connection.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := connection.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err := connection.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || synchronous != 2 || busyTimeout != 5000 {
			t.Fatalf("connection %d pragmas = foreign_keys:%d synchronous:%d busy_timeout:%d", index, foreignKeys, synchronous, busyTimeout)
		}
	}
}
