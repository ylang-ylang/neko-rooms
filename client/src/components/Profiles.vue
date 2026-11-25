<template>
  <span>
    <v-btn outlined small :disabled="!storageEnabled" @click="open = true; Load()">Profiles</v-btn>
    <v-dialog v-model="open" max-width="720px" :persistent="busy">
      <v-card>
        <v-card-title>Browser profiles</v-card-title>
        <v-card-text>
          <p>Profiles retain browser identity and Downloads after a deployment is removed. A profile can be bound to only one deployment, including while stopped. Remove the deployment to release it.</p>
          <v-form @submit.prevent="Create">
            <div class="d-flex align-center">
              <v-text-field v-model="name" label="New profile name" maxlength="120" :disabled="busy" class="mr-3" />
              <v-btn type="submit" color="primary" :disabled="busy || !name.trim()" :loading="busy">Create</v-btn>
              <v-btn icon :disabled="busy" aria-label="Refresh profiles" @click="Load"><v-icon>mdi-refresh</v-icon></v-btn>
            </div>
          </v-form>
          <v-alert v-if="error" type="error" dense outlined role="alert">{{ error }}</v-alert>
          <v-progress-linear v-if="loading" indeterminate />
          <v-simple-table>
            <thead><tr><th>Profile</th><th>Binding</th><th>Delete data</th></tr></thead>
            <tbody>
              <tr v-for="profile in profiles" :key="profile.id">
                <td class="text-break py-2">{{ profile.name }}<div class="text-caption">{{ profile.id }}</div></td>
                <td class="text-break">{{ profile.room_id ? 'Bound to ' + profile.room_name : 'Available' }}</td>
                <td><v-btn icon color="error" :disabled="busy || !!profile.room_id" :aria-label="'Permanently delete profile ' + profile.name" @click="Delete(profile)"><v-icon>mdi-delete-outline</v-icon></v-btn></td>
              </tr>
            </tbody>
          </v-simple-table>
          <p v-if="!loading && !profiles.length" class="mt-3">No profiles yet. New deployments create a durable profile automatically.</p>
        </v-card-text>
        <v-card-actions><v-spacer /><v-btn text :disabled="busy" @click="open = false">Close</v-btn></v-card-actions>
      </v-card>
    </v-dialog>
  </span>
</template>

<script lang="ts">
import { Component, Vue } from 'vue-property-decorator'
import axios from 'axios'
import { BrowserProfile } from '@/api'

@Component
export default class Profiles extends Vue {
  public open = false
  public busy = false
  public loading = false
  public name = ''
  public error = ''

  get storageEnabled() { return this.$store.state.roomsConfig.storage_enabled }
  get profiles(): BrowserProfile[] { return this.$store.state.profiles }

  failure(error: unknown): string {
    return axios.isAxiosError(error) && error.response ? String(error.response.data).trim() : 'Network request failed. Refresh profiles before retrying.'
  }

  async Load() {
    this.loading = true
    this.error = ''
    try { await this.$store.dispatch('PROFILES_LOAD') }
    catch (error) { this.error = this.failure(error) }
    finally { this.loading = false }
  }

  async Create() {
    if (this.busy || !this.name.trim()) return
    this.busy = true
    this.error = ''
    try {
      await this.$store.dispatch('PROFILES_CREATE', this.name.trim())
      this.name = ''
      await this.Load()
    } catch (error) { this.error = this.failure(error) }
    finally { this.busy = false }
  }

  async Delete(profile: BrowserProfile) {
    if (this.busy || profile.room_id) return
    const result = await this.$swal({
      title: 'Permanently delete profile?',
      text: `Delete "${profile.name}" (${profile.id}) and all its cookies, history, extensions, site data and Downloads? This cannot be undone.`,
      icon: 'warning', showCancelButton: true,
      confirmButtonText: 'Delete profile and data', cancelButtonText: 'Cancel',
    })
    if (!result.isConfirmed) return
    this.busy = true
    this.error = ''
    try {
      await this.$store.dispatch('PROFILES_DELETE', profile.id)
      await this.Load()
    } catch (error) { this.error = this.failure(error) }
    finally { this.busy = false }
  }
}
</script>
