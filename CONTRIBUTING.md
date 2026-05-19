# Contributing to grok2api

Thank you for contributing to grok2api!

## Development Setup

1. **Requirements**
   - Python 3.10+
   - FastAPI
   - Docker (optional, for containerized deployment)

2. **Clone and install**
   ```bash
   git clone https://github.com/chenyme/grok2api.git
   cd grok2api
   pip install -e .
   ```

3. **Configuration**
   ```bash
   cp .env.example .env
   # Edit .env with your API keys
   ```

4. **Run the server**
   ```bash
   uvicorn main:app --reload
   # or
   python -m grok2api
   ```

## Project Structure

```
grok2api/
├── app/                 # Main application
│   ├── api/             # API routes
│   ├── core/            # Core logic
│   └── models/          # Data models
├── tests/               # Test suite
├── docker/              # Docker configuration
└── docs/                # Documentation
```

## Making Changes

1. **Create a branch**
   ```bash
   git checkout -b feat/your-feature
   ```

2. **Code style**
   - Follow PEP 8
   - Use type hints
   - Add docstrings

3. **Test**
   ```bash
   pytest tests/
   ```

4. **Commit and push**
   ```bash
   git commit -m "feat: description"
   git push
   ```

## Pull Request Process

1. Fork the repository
2. Create feature branch
3. Make changes with tests
4. Submit PR with description

## Security

If you find a security vulnerability, please see SECURITY.md for reporting instructions.

## License

By contributing, you agree your contributions are licensed under the project license.
