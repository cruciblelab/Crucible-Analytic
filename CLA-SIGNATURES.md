# CLA signatures

Everyone who has agreed to `CLA.md`.

Add yourself in your first pull request, one row, in this format:

```
| Full name | GitHub username | Date (YYYY-MM-DD) | Individual / on behalf of employer |
```

Signing once covers your later contributions.

| Name | GitHub | Date | Capacity |
|---|---|---|---|
| Fırat Coşkun | @cruciblelab | 2026-08-31 | Project owner |
| CrucibleLAB | @cruciblelab | 2026-08-31 | The same person, under the account name that authored the repository's first commit |

---

## Why the owner is on the list

The owner holds the copyright and does not need to license work to
themselves. The row is here because a signature file that lists only
other people invites the reading that the rules are for contributors and
not for the person collecting them.

## Why one person has two rows

`internal/docs/cla_test.go` compares this table against the names git
records as commit authors, and the history carries both: the repository's
first commit was made by GitHub itself under the account name
`CrucibleLAB`, and everything after it is authored by the person behind
that account.

The first commit was deliberately left as it is. Rewriting it would
change its hash, and it is the only commit this branch shares with
`main` — the shared ancestor that makes the two comparable at all. The
alternative to a second row here was rewriting history to remove a name
that is not wrong, in order to tidy a table. That is the wrong way round.
