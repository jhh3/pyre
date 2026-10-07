# jhh3-mem credential redaction ruleset (SPEC §12).
# Applied with `sed -E -f` to EVERY string on EVERY write path:
# capture, note, enrich, and migration alike.
#
# Design rules:
#   - Redact VALUES, preserve NAMES, so `signing_key` stays FTS-searchable.
#   - Match token *shapes*, never English words; no bare-hex rule, so commit
#     SHAs and flag ids survive intact.
#   - ORDER MATTERS: the userinfo rule must run before the key/value rule.
#   - Replacement text contains no quote characters, so a redacted line that is
#     part of a generated SQL statement stays syntactically valid.
# Portability: POSIX ERE has no \b (GNU-only; BSD/macOS sed silently treats it
# as a literal and redacts nothing). Word boundaries are spelled
# (^|[^A-Za-z0-9_]) and re-emitted; rules with a trailing boundary run twice
# so adjacent tokens that share one separator are both caught.

# 1. URL userinfo — caught all 164 live postgres credential rows in the corpus.
#    Preserves host and port: the debugging value survives, the credential does not.
#    The {3,} length gate is what stops this mangling a bare host:port.
s#(://[^:/@[:space:]]+):[^@[:space:]]{3,}@#\1:[REDACTED:userinfo]@#g

# 2. Anthropic API keys — keeps a 6-char prefix so the key is identifiable but unusable.
s#(sk-ant-[A-Za-z0-9_-]{6})[A-Za-z0-9_-]{12,}#\1[REDACTED:anthropic]#g

# 3. AWS access key ids. ASIA is what every `aws sso login` / assume-role
#    session mints and is now the more common shape than the long-lived AKIA.
s#(^|[^A-Za-z0-9_])A(KIA|SIA|BIA|CCA|GPA|IDA|IPA|NPA|NVA|PKA|ROA|SCA)[0-9A-Z]{16}([^A-Za-z0-9_]|$)#\1[REDACTED:aws]\3#g
s#(^|[^A-Za-z0-9_])A(KIA|SIA|BIA|CCA|GPA|IDA|IPA|NPA|NVA|PKA|ROA|SCA)[0-9A-Z]{16}([^A-Za-z0-9_]|$)#\1[REDACTED:aws]\3#g

# 4. GitHub tokens (ghp_ gho_ ghu_ ghs_ ghr_), and the current default shape,
#    the fine-grained `github_pat_` PAT.
s#(^|[^A-Za-z0-9_])(gh[pousr]_)[A-Za-z0-9]{30,}#\1\2[REDACTED:github]#g
s#(^|[^A-Za-z0-9_])github_pat_[A-Za-z0-9_]{20,}#\1[REDACTED:github]#g

# 5. Slack tokens.
s#(^|[^A-Za-z0-9_])xox[baprse]-[A-Za-z0-9-]{20,}#\1[REDACTED:slack]#g

# 6. JWTs. Length gates leave short fake/example tokens alone.
s#(^|[^A-Za-z0-9_])eyJ[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{10,}#\1[REDACTED:jwt]#g

# 7. PEM private keys. THREE rules, in this order, because sed is line-oriented
#    and the key arrives in two different shapes:
#
#    7a. The FLATTENED shape, which is what capture actually sees: _jm_cap_enc
#        turns every LF into a literal backslash-n before redaction runs, so a
#        pasted key is one long line. Greedy `.*` is correct here — everything
#        between the first BEGIN and the last END on a line is key material.
s#-----BEGIN [A-Z ]*PRIVATE KEY-----.*-----END [A-Z ]*PRIVATE KEY-----#[REDACTED:privkey]#g
#    7b. The genuinely MULTI-LINE shape (a file, a stream). Inside a BEGIN..END
#        range, blank any line that is nothing but base64. Restricting it to
#        whole-line base64 is what makes an unterminated block safe: it can
#        never eat a TSV record or a line of prose, only key body.
/-----BEGIN [A-Z ]*PRIVATE KEY-----/,/-----END [A-Z ]*PRIVATE KEY-----/ s#^[A-Za-z0-9+/]{20,}={0,2}[[:space:]]*$#[REDACTED:privkey]#
#    7c. The surviving header/footer lines. MUST come after 7b: this rewrites
#        the BEGIN line, and 7b's range address would then never open.
s#-----(BEGIN|END) [A-Z ]*PRIVATE KEY-----#[REDACTED:privkey]#g

# 8. Google API keys.
s#(^|[^A-Za-z0-9_])(AIza)[A-Za-z0-9_-]{35}([^A-Za-z0-9_]|$)#\1[REDACTED:gcp]\3#g
s#(^|[^A-Za-z0-9_])(AIza)[A-Za-z0-9_-]{35}([^A-Za-z0-9_]|$)#\1[REDACTED:gcp]\3#g

# 8a. OpenAI. The project/service-account shapes first, then the classic
#     `sk-<48>`. Ordered AFTER rule 2 so `sk-ant-` keeps its prefix-preserving
#     treatment rather than being swallowed by the generic form.
s#(^|[^A-Za-z0-9_])sk-(proj|svcacct|admin)-[A-Za-z0-9_-]{20,}#\1[REDACTED:openai]#g
s#(^|[^A-Za-z0-9_])sk-[A-Za-z0-9]{32,}#\1[REDACTED:openai]#g

# 8b. HTTP Authorization headers — the single most common way a live credential
#     appears in a debugging paste. The scheme is preserved; the secret is not.
s#(([Aa]uthorization|AUTHORIZATION)["']?[[:space:]]*:[[:space:]]*(Bearer|BEARER|bearer|Basic|BASIC|basic|Token|TOKEN|token)[[:space:]]+)[A-Za-z0-9._~+/=-]{12,}#\1[REDACTED:authz]#g

# 8c. Other vendor tokens with an unambiguous prefix.
s#(^|[^A-Za-z0-9_])(glpat-|npm_|dckr_pat_|hf_|shpat_|shpss_|sq0csp-|rk_live_|sk_live_|pk_live_)[A-Za-z0-9_-]{16,}#\1[REDACTED:vendor]#g

# 8d. SendGrid.
s#(^|[^A-Za-z0-9_])SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}#\1[REDACTED:sendgrid]#g

# 8e. Slack incoming-webhook URLs. The path IS the credential; the host is kept
#     so the debugging value ("we post to Slack here") survives.
s#https://hooks\.slack\.com/services/[A-Za-z0-9/+_-]{20,}#https://hooks.slack.com/services/[REDACTED:webhook]#g

# 9. key=value / key: value credentials. Redacts the VALUE, preserves the NAME —
#    SIGNING_KEY=abc123 becomes SIGNING_KEY=[REDACTED:kv], keeping `signing_key`
#    as a high-precision searchable term. Case-insensitive (GNU sed `I` flag).
#
#    DEVIATION FROM SPEC §12, deliberate and minimal: the spec's alternation is
#    (password|passwd|secret|token|api[_-]?key|access[_-]?key|client[_-]?secret),
#    which cannot match its own documented example `SIGNING_KEY=...` and so fails
#    the spec's own required test RED-5. Four unambiguously-credential names are
#    added: signing/secret/private/encryption key. Deliberately NOT added: a bare
#    `[a-z]+[_-]key`, which would swallow cache_key / partition_key / object_key
#    and destroy debugging identifiers for no security gain.
s#((password|passwd|secret|token|api[_-]?key|access[_-]?key|client[_-]?secret|signing[_-]?key|secret[_-]?key|private[_-]?key|encryption[_-]?key)["']?[[:space:]]*[:=][[:space:]]*["']?)[^[:space:]"',;]{8,}#\1[REDACTED:kv]#gI
