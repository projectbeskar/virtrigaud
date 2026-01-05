# Building the Documentation

This directory contains the mdbook configuration for the VirtRigaud documentation.

## Prerequisites

Install mdbook:

```bash
# macOS
brew install mdbook

# Linux
cargo install mdbook

# Or download from releases
# https://github.com/rust-lang/mdBook/releases
```

## Building the Documentation

```bash
# Build the documentation
cd docs
mdbook build

# Serve with live reload for development
mdbook serve

# Open in browser (default: http://localhost:3000)
mdbook serve --open
```

## Structure

- `book.toml` - mdbook configuration
- `src/SUMMARY.md` - Table of contents
- `src/` - All documentation files (symlinked from parent directory)
- `book/` - Build output (gitignored)

## Deployment

The documentation can be deployed to:
- GitHub Pages
- GitLab Pages
- Netlify
- Any static hosting service

The built documentation is in the `book/` directory after running `mdbook build`.

## Live Development

When making changes to documentation files, run `mdbook serve` to see changes in real-time at http://localhost:3000.
