# Spanner provider documentation

These pages and the repository [README](../README.md) document what Spanner
does in Ptah: which statements a plan writes, which changes the provider plans
and which it refuses, how a migration runs, and what is not supported yet.
Ptah's own documentation, at [docs.ptah.run](https://docs.ptah.run/edge/),
describes the commands, the source formats and the pipeline every engine
shares; its
[Configure the Spanner provider](https://docs.ptah.run/edge/extend/providers/spanner/)
page installs and selects this provider.

| Topic | Where |
| --- | --- |
| Installing the provider, the engine name, connection addresses, dev databases and `ptah-compat` | [Using the Spanner provider with Ptah](usage.md) |
| Tables, column types, keys, indexes, constraints, row deletion policies, views, sequences, schemas and declared rows | [Declaring Spanner objects](declarations.md) |
| Migration bodies and transactions, history tables, the migration lock, dev database cleanup, lint and seeds | [Migrations on Spanner](migrations.md) |
| What the provider refuses, what it does not describe yet, and where the emulator differs | [What the Spanner provider refuses](limitations.md) |
| Releases, platforms and the minimum contract | [Releases](../README.md#releases) |
| Building the provider and running its tests | [Build and test](../README.md#build-and-test) |

Support is best effort. The provider's live tests run the Cloud Spanner
emulator behind PGAdapter, which is evidence about Spanner's PostgreSQL
interface, not about the managed service. The release line the provider
declares is published in each release's `release-lines.json`; Ptah's support
matrix lists only the engines Ptah builds in.

## How these pages reach docs.ptah.run

Every page in this folder except this README is published on docs.ptah.run
under `extend/providers/spanner/<page>/`, beside Ptah's own pages, and
[Configure the Spanner provider](https://docs.ptah.run/edge/extend/providers/spanner/)
introduces them. The pages live here so that a change to the provider and a
change to its documentation are one pull request.

Ptah's documentation build imports this folder at the release of this provider
that `docs/site/scripts/data/provider-docs.json` in stokaro/ptah names. A merged
change here therefore reaches the site when Ptah declares a release that
carries it: the pull request that moves the declaration to the new release
imports its pages, records any new page's address, and adds a redirect for a
page this folder removed or renamed. Each released version of Ptah's
documentation keeps the pages of the release it declared.

A page here is written for both places:

- It starts with the frontmatter every page of the site carries: `title`,
  `description`, `type`, `audience`, `readerQuestion`, `goal`,
  `sourceOfTruth`, `generated: false`, `overlaps` and `disposition`.
  `sidebar.order` places it in the provider's part of the navigation.
- Its `# Title` heading repeats `title`, so the page reads on GitHub; the site
  renders the title from the frontmatter and drops the heading.
- It links another page of this folder by its file name, any other file of this
  repository by its relative path, and a Ptah page by its address on
  `https://docs.ptah.run/edge/`. The import turns each into an address that
  works on the site: a route of the site, or the file on GitHub at the imported
  commit. `sourceOfTruth` names paths relative to the repository root.

Ptah's
[documentation site README](https://github.com/stokaro/ptah/blob/master/docs/site/README.md#provider-documentation)
lists what the import accepts and refuses.

`.github/workflows/docs.yml` runs Ptah's documentation checks on this folder in
every pull request that changes it, from Ptah's own `provider-docs.yml`
workflow: the style, terminology and link checks, the command lines a page
publishes, and a build of the whole site with these pages in it. The checkers
stay in Ptah. To read a page on the site before a release declares it, import
this checkout into a Ptah checkout:

```console
cd docs/site
npm ci
node scripts/import-provider-docs.mjs --source spanner=/path/to/ptah-spanner-plugin
npm run dev
```

The import refuses an engine that `provider-docs.json` does not declare, so
until Ptah declares this provider, add a local entry for `spanner` to that file
in the Ptah checkout first and leave it uncommitted.
