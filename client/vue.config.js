const { defineConfig } = require('@vue/cli-service')
module.exports = defineConfig({
  productionSourceMap: false,
  transpileDependencies: [
    'vuetify'
  ],
  // Keep the Owner Manager app's existing relative asset URLs.
  publicPath: './',
  assetsDir: './',
  pages: {
    app: {
      entry: 'src/main.ts',
      template: 'public/index.html',
      filename: 'index.html',
      chunks: ['chunk-vendors', 'chunk-common', 'app'],
    },
    aggregate: {
      entry: 'src/aggregate/main.ts',
      template: 'src/aggregate/index.html',
      filename: 'aggregate.html',
      chunks: ['aggregate-vendors', 'aggregate'],
      // Backend serves this HTML at /room/<name>/, but assets are always public here.
      publicPath: '/__aggregate/',
    },
  },
  chainWebpack(config) {
    // Do not make the public room page download the Owner/Vue app's vendor graph.
    const groups = config.optimization.get('splitChunks').cacheGroups
    groups.defaultVendors.chunks = chunk => chunk.name !== 'aggregate'
    groups.common.chunks = chunk => chunk.name !== 'aggregate'
    groups.aggregateVendors = {
      name: 'aggregate-vendors',
      test: /[\\/]node_modules[\\/]/,
      chunks: chunk => chunk.name === 'aggregate',
      priority: 10,
    }
  },
  devServer: {
    allowedHosts: "all",
    proxy: process.env.API_PROXY ? {
      '^/(api|auth)(/|$)': {
        target: process.env.API_PROXY,
        timeout: 0, // because of SSE
      },
    } : undefined,
    compress: false, // because of SSE
  }
})
