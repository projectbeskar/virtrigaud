# Translation Guide for VirtRigaud Documentation

VirtRigaud documentation uses mdBook with multilingual support. This guide explains how to add translations to the documentation.

## Current Languages

- **English (en)**: Primary language in `docs/src/`

## Adding a New Language Translation

### Step 1: Create Language Directory

Create a new directory for your language using ISO 639-1 language codes:

```bash
# Example for French
cp -r docs/src docs/src-fr

# Example for Spanish
cp -r docs/src docs/src-es

# Example for Japanese
cp -r docs/src docs/src-ja
```

### Step 2: Translate Content

Translate all `.md` files in your language directory:

1. Start with `SUMMARY.md` (table of contents)
2. Translate `readme.md` (introduction)
3. Translate all documentation pages
4. Keep code examples in English unless localization adds value
5. Keep file names in English for consistency

**Translation Guidelines:**

- Maintain the same directory structure
- Keep all filenames in lowercase English, **EXCEPT `SUMMARY.md`** (must be uppercase for mdBook)
- Preserve markdown formatting and links
- Keep technical terms consistent
- Update links to other pages if needed

**Important:** mdBook requires the table of contents to be named `SUMMARY.md` (uppercase). All other files should be lowercase.

### Step 3: Update book.toml

Add your language configuration to `docs/book.toml`:

```toml
[language.fr]
name = "Français"
src = "src-fr"

[language.es]
name = "Español"
src = "src-es"

[language.ja]
name = "日本語"
src = "src-ja"
```

### Step 4: Update CI Workflow

The CI workflow in `.github/workflows/docs.yml` will automatically detect and build all configured languages.

### Step 5: Test Locally

Build and test your translation:

```bash
# Build English (default)
cd docs && mdbook build

# Build specific language
cd docs && mdbook build -d book/fr

# Serve locally to preview
mdbook serve
```

### Step 6: Create Pull Request

Submit your translation:

1. Create a feature branch: `git checkout -b add-french-translation`
2. Commit your changes: `git commit -m "Add French translation"`
3. Push to GitHub: `git push origin add-french-translation`
4. Create a Pull Request with:
   - Clear description of the language added
   - List of translated pages
   - Any language-specific considerations

## Language Maintenance

### Keeping Translations Updated

When the English documentation changes:

1. Check the `git diff` for modified files
2. Update corresponding files in language directories
3. Mark outdated translations with a notice at the top:

```markdown
> ⚠️ **Translation Status**: This page was last updated on 2024-12-02.
> Some content may be outdated. See [English version](../readme.md) for latest.
```

### Translation Quality

- Use consistent terminology across all pages
- Follow the language's style guide
- Have native speakers review translations
- Test all links and examples

## Supported Languages (ISO 639-1 Codes)

Common language codes for reference:

| Code | Language | Native Name |
|------|----------|-------------|
| en | English | English |
| fr | French | Français |
| es | Spanish | Español |
| de | German | Deutsch |
| ja | Japanese | 日本語 |
| zh | Chinese | 中文 |
| ko | Korean | 한국어 |
| pt | Portuguese | Português |
| ru | Russian | Русский |
| ar | Arabic | العربية |

## Example: Adding French Translation

```bash
# 1. Create French directory
cp -r docs/src docs/src-fr

# 2. Translate files
vim docs/src-fr/readme.md
vim docs/src-fr/summary.md
# ... translate all files ...

# 3. Add to book.toml
cat >> docs/book.toml << 'EOF'

[language.fr]
name = "Français"
src = "src-fr"
EOF

# 4. Test build
cd docs && mdbook build -d book/fr

# 5. Verify
open book/fr/index.html
```

## Automation and Tools

### Translation Memory

Consider using:
- **Crowdin**: Translation management platform
- **Weblate**: Open-source translation tool
- **mdbook-i18n**: mdBook internationalization preprocessor

### Automated Checks

The CI pipeline automatically:
- Builds all configured languages
- Validates markdown syntax
- Checks for broken links
- Generates language-specific builds

## Getting Help

- Open an issue: https://github.com/projectbeskar/virtrigaud/issues
- Join discussions: https://github.com/projectbeskar/virtrigaud/discussions
- Translation team: docs-translation@virtrigaud.io (if available)

## Credits

Thank you to all translators who help make VirtRigaud accessible globally!

To add your name to the contributors list, update `docs/book.toml` authors section.
