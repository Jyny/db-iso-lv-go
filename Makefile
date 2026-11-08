# Experiments share one PostgreSQL and one MySQL server. Some metrics (e.g. InnoDB's
# history list length) are server-wide, so packages run serially (-p 1).
GOTEST = go test -p 1 -count=1

.PHONY: up down wait test test-v hermitage postgres mysql psql mysql-cli

up:
	docker-compose up -d
	@$(MAKE) --no-print-directory wait

down:
	docker-compose down -v

wait:
	@until docker exec lab-postgres pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	@until docker exec lab-mysql mysqladmin ping -h 127.0.0.1 -plab >/dev/null 2>&1; do sleep 1; done
	@echo "postgres + mysql ready"

test:
	$(GOTEST) ./...

# Full timelines for every experiment.
test-v:
	$(GOTEST) -v ./...

# Hermitage isolation matrix; the summary table is printed at the end.
hermitage:
	$(GOTEST) -v ./hermitage | tail -15

postgres:
	$(GOTEST) -v ./postgres

mysql:
	$(GOTEST) -v ./mysql

psql:
	docker exec -it lab-postgres psql -U postgres lab

mysql-cli:
	docker exec -it lab-mysql mysql -uroot -plab lab
