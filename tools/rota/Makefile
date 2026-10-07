BIN := $(HOME)/.local/bin/rota
EXPORT_FILE := accounts.export.json

.PHONY: install uninstall test export import

install:
	install -D -m 755 rota $(BIN)
	@echo "installed $(BIN)"

uninstall:
	rm -f $(BIN)
	@echo "removed $(BIN)"

test:
	python3 -m unittest -v test_rota.py

# Write account snapshots (OAuth secrets!) to a gitignored, owner-only file.
export:
	@umask 077 && $(BIN) export > $(EXPORT_FILE)
	@echo "wrote $(EXPORT_FILE) (chmod 600, gitignored — do not commit)"

# Import snapshots from $(EXPORT_FILE); use FORCE=1 to overwrite existing.
import:
	$(BIN) import $(EXPORT_FILE) $(if $(FORCE),--force,)
