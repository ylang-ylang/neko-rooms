import Vue from 'vue'
import App from './App.vue'
import './plugins/filters.ts'
import sweetalert from './plugins/sweetalert'
import store from './store'
import vuetify from './plugins/vuetify'
import { initializeManagerAuth, ManagerAuthStatus } from './auth'

import '@/assets/styles/main.scss';

Vue.config.productionTip = false

Vue.use(sweetalert)

async function bootstrap() {
  let auth: ManagerAuthStatus | null
  try {
    auth = await initializeManagerAuth()
  } catch (error) {
    const app = document.getElementById('app')
    if (app) {
      app.setAttribute('role', 'alert')
      app.textContent = '无法读取管理登录状态，管理界面未启动。请刷新页面重试。' +
        (error instanceof Error ? ' ' + error.message : '')
    }
    return
  }
  if (!auth) return

  new Vue({
    store,
    vuetify,
    render: h => h(App, { props: { auth } })
  }).$mount('#app')
}

bootstrap()
