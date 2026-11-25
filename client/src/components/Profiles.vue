<template>
  <span>
    <v-btn outlined small :disabled="!storageEnabled" @click="open = true; Load()">Profiles</v-btn>
    <v-dialog v-model="open" max-width="720px" :persistent="busy" scrollable>
      <v-card class="profile-card">
        <v-card-title>Browser profiles</v-card-title>
        <v-card-text class="profile-content">
          <p>Profiles keep browser data and Downloads after a deployment is removed. Each profile can be bound to one deployment.</p>
          <v-form @submit.prevent="Create">
            <div class="profile-create">
              <v-text-field v-model="name" label="New profile name" maxlength="120" :disabled="busy" hide-details="auto" class="profile-name-input" />
              <v-btn type="submit" color="primary" :disabled="busy || !name.trim()" :loading="busy">Create</v-btn>
              <v-btn icon :disabled="busy" aria-label="Refresh profiles" @click="Load"><v-icon>mdi-refresh</v-icon></v-btn>
            </div>
          </v-form>
          <v-alert v-if="error" type="error" dense outlined role="alert">{{ error }}</v-alert>
          <v-progress-linear v-if="loading" indeterminate />
          <table class="profile-table">
            <thead><tr><th scope="col">Profile</th><th scope="col">Binding</th><th scope="col">Delete data</th></tr></thead>
            <tbody>
              <tr v-for="profile in profiles" :key="profile.id">
                <td>
                  <div class="profile-name" :title="profile.name">{{ profile.name }}</div>
                  <div class="profile-id" :title="profile.id">ID {{ profile.id.slice(0, 8) }}{{ profile.id.length > 8 ? '…' : '' }}</div>
                </td>
                <td>
                  <v-chip x-small outlined :color="profile.room_id ? undefined : 'teal lighten-2'" class="profile-state">{{ profile.room_id ? 'Bound' : 'Available' }}</v-chip>
                  <div v-if="profile.room_id" class="profile-binding" :title="'Bound to ' + profile.room_name + '. Remove this deployment before reusing the profile.'">{{ profile.room_name }}</div>
                </td>
                <td class="profile-delete" :title="profile.room_id ? 'Remove the bound deployment before deleting its profile.' : 'Permanently delete this profile and its data.'"><v-btn icon color="error" :disabled="busy || !!profile.room_id" :aria-label="'Permanently delete profile ' + profile.name" @click="Delete(profile)"><v-icon>mdi-delete-outline</v-icon></v-btn></td>
              </tr>
            </tbody>
          </table>
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

<style scoped>
.profile-card {
  max-height: calc(100vh - 48px);
  max-height: calc(100dvh - 48px);
}
.profile-content { min-height: 0; }
.profile-create {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto auto;
  align-items: center;
  gap: 12px;
  margin-bottom: 20px;
}
.profile-name-input { min-width: 0; }
.profile-table { width: 100%; border-collapse: collapse; table-layout: fixed; }
.profile-table th { padding: 8px 10px; text-align: left; font-size: 12px; }
.profile-table th:first-child { width: 52%; }
.profile-table th:nth-child(2) { width: 34%; }
.profile-table th:last-child { width: 14%; text-align: right; }
.profile-table td { padding: 14px 10px; vertical-align: top; border-top: 1px solid rgba(255, 255, 255, .12); }
.profile-name, .profile-binding { white-space: nowrap; text-overflow: ellipsis; overflow: hidden; line-height: 1.4; }
.profile-name { font-weight: 600; }
.profile-id { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 11px; opacity: .65; margin-top: 4px; }
.profile-state { font-weight: 500; }
.profile-binding { margin-top: 3px; font-size: 12px; opacity: .75; }
.profile-table .profile-delete { text-align: right; padding-top: 8px; }
@media (max-width: 600px) {
  .profile-create { grid-template-columns: minmax(0, 1fr) auto; gap: 8px; }
  .profile-name-input { grid-column: 1 / -1; }
  .profile-create > .v-btn { justify-self: end; }
  .profile-table, .profile-table tbody { display: block; }
  .profile-table thead { display: none; }
  .profile-table tr { display: grid; grid-template-columns: minmax(0, 1fr) 40px; gap: 6px 8px; padding: 14px 0; border-top: 1px solid rgba(255, 255, 255, .12); }
  .profile-table td { display: block; min-width: 0; padding: 0; border: 0; }
  .profile-table td:first-child { grid-column: 1; grid-row: 1; }
  .profile-table td:nth-child(2) { grid-column: 1; grid-row: 2; }
  .profile-table .profile-delete { grid-column: 2; grid-row: 1 / span 2; padding: 0; }
}
</style>
