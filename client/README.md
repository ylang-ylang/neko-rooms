# client

## Project setup
```
npm install
```

### Compiles and hot-reloads for development
```
npm run serve
```

### Compiles and minifies for production
```
npm run build
```

### Lints and fixes files
```
npm run lint
```

### Clone-room API sync and tests

After updating the `roomClone` operation in `../OpenApi.yml`, run:
```
npm run generate:clone
npm run test:clone
npm run test:rooms
npm run test:auth
```

This bounded generation updates only the clone request model and operation in
`src/api/api.ts`, reusing the existing SDK transport templates. It preserves
handwritten Agent APIs, Owner authentication and network metadata. Do not run the
full API-directory replacement just to sync this operation.

### Customize configuration
See [Configuration Reference](https://cli.vuejs.org/config/).
