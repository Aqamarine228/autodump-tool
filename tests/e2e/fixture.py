"""Generate an e2e fixture: `database`.parsing_documents + `database_documents`.document_N.

Usage: python3 fixture.py DOCS ROWS_PER_DOC BLOCKER_ID BLOCKER_AGE_HOURS > fixture.sql
BLOCKER_ID=0 means every document is ready.
"""
import random
import sys

docs, rows, blocker, age = (int(a) for a in sys.argv[1:5])
rnd = random.Random(42)
out = sys.stdout.write

out("DROP DATABASE IF EXISTS `database`; CREATE DATABASE `database`;\n")
out("DROP DATABASE IF EXISTS `database_documents`; CREATE DATABASE `database_documents`;\n")
out("""CREATE TABLE `database`.parsing_documents (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  agent_id BIGINT UNSIGNED NOT NULL, done TINYINT(1) NOT NULL DEFAULT 0,
  total_orders INT NOT NULL DEFAULT 0, finished_orders INT NOT NULL DEFAULT 0,
  created_at DATETIME NULL, updated_at DATETIME NULL) ENGINE=InnoDB;\n""")

statuses = ["ok", "error", "нет в наличии", "🙂 promo", "it's \"quoted\" \\ slash"]
for i in range(1, docs + 1):
    empty = i % 50 == 0  # every 50th document has no orders
    done = 0 if i == blocker else 1
    created = f"NOW() - INTERVAL {age} HOUR" if i == blocker else "NOW() - INTERVAL 2 DAY"
    total = 0 if empty else rows
    out(f"INSERT INTO `database`.parsing_documents VALUES ({i},{i % 7},{done},{total},{total},{created},{created});\n")
    out(f"""CREATE TABLE `database_documents`.document_{i} (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  created_at DATETIME NULL, updated_at DATETIME NULL,
  comparison_id BIGINT UNSIGNED, product_id BIGINT UNSIGNED, domen_id BIGINT UNSIGNED,
  rrc DOUBLE NULL, price DECIMAL(12,2) NULL, old_price DOUBLE NULL, cashback FLOAT NULL, promo DOUBLE NULL,
  need_confirmed TINYINT(1) NOT NULL DEFAULT 0, demping TINYINT(1) NOT NULL DEFAULT 0,
  status VARCHAR(32) NOT NULL DEFAULT '', availability VARCHAR(255) NULL, inner_code VARCHAR(255) NULL,
  delivery VARCHAR(255) NULL, warranty VARCHAR(255) NULL, additional_field VARCHAR(255) NULL,
  screenshot TEXT NULL, is_available TINYINT(1) NOT NULL DEFAULT 0,
  KEY idx_product (product_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n""")
    if empty:
        continue
    vals = []
    for r in range(rows):
        price = "NULL" if r % 9 == 0 else f"{rnd.uniform(-10, 99999):.2f}"
        rrc = "NULL" if r % 11 == 0 else repr(rnd.uniform(0, 1e6))
        cash = "NULL" if r % 5 == 0 else repr(rnd.uniform(0, 500))
        st = statuses[r % len(statuses)].replace("\\", "\\\\").replace("'", "\\'")
        vals.append(
            f"(NULL,NOW(),NOW(),{rnd.randint(1, 10**9)},{rnd.randint(1, 10**6)},{rnd.randint(1, 500)},"
            f"{rrc},{price},{repr(rnd.random() * 1000)},{cash},NULL,{r % 2},{r % 3 == 0},'{st}',"
            f"'в наличии',NULL,'1-2 дня',NULL,'line1\\nline2','https://example.com/s/{i}/{r}.png',{r % 2})"
        )
    for k in range(0, len(vals), 500):
        out(f"INSERT INTO `database_documents`.document_{i} VALUES " + ",".join(vals[k:k + 500]) + ";\n")
