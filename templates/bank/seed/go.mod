module bank/seed

go 1.22.7

require (
	bank/core-banking v0.0.0
	bank/customer v0.0.0
	bank/lib v0.0.0
	bank/loan v0.0.0
	bank/payment v0.0.0
	bank/reward v0.0.0
	bank/risk v0.0.0
	bank/wealth v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20221227161230-091c0ba34f0a // indirect
	github.com/jackc/pgx/v5 v5.5.5 // indirect
	github.com/jackc/puddle/v2 v2.2.1 // indirect
	github.com/stretchr/testify v1.9.0 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/sync v0.10.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)

replace bank/core-banking => ../core-banking

replace bank/customer => ../customer

replace bank/lib => ../lib

replace bank/loan => ../loan

replace bank/payment => ../payment

replace bank/reward => ../reward

replace bank/risk => ../risk

replace bank/wealth => ../wealth

replace bank => ../contracts
