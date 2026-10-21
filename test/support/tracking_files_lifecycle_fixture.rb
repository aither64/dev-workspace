# frozen_string_literal: true

# This helper runs only in a Go test's disposable workspace. Reuse the normal
# Ruby fixture observation adapter; archive/revive themselves are not stubbed.
require_relative 'dev_session_test_case'

class DevSessionTest
  TODAY = Date.new(2026, 10, 22)

  class TrackingFilesTmux
    def argv(*args) = ['tmux', *args]
    def session(_slug) = nil
    def session_by_id(_id) = nil
    def managed_session_identities = []
    def current_session_identity(_pane) = nil
    def pane_session_id(_pane) = nil
    def pane_current_command(_pane) = nil
  end
end

workspace = File.realpath(ARGV.fetch(0))
phase = ARGV.fetch(1)
slug = '2026-10-22-tracking-files-fixture'
fixture = DevSessionTest.new('tracking_files_fixture')
runner = fixture.send(:archive_runner_for, workspace,
                      tmux: DevSessionTest::TrackingFilesTmux.new,
                      env: { 'DEV_WORKSPACE_DEFER_TRACKING' => '1' })
case phase
when 'initialize'
  %w[repos work worktrees].each { |name| FileUtils.mkdir_p(File.join(workspace, name)) }
  runner.ensure_tracking_files(slug)
  runner.send(:ensure_portal_manifest, slug, tracking_only: true)
  fixture.send(:commit_archive_fixture, workspace, slug, lifecycle: 'active')
  fixture.send(:configure_workspace_origin, workspace)
when 'archive'
  runner.archive(slug, as_is: true, explicit: true)
  fixture.assert(File.directory?(File.join(workspace, 'archive', slug)))
  fixture.refute(File.exist?(File.join(workspace, 'work', slug)))
  fixture.assert_equal('archived', runner.send(:lifecycle_state,
                      File.read(File.join(workspace, 'archive', slug, 'state.md'))))
  fixture.refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
  fixture.refute(File.exist?(DevSession::ArchiveCleanup.new(runner, slug).path))
  fixture.assert_equal('pending', runner.tracking_sync_receipt(slug).fetch('state'))
when 'revive'
  runner.revive(slug, as_is: true)
  fixture.assert(File.directory?(File.join(workspace, 'work', slug)))
  fixture.refute(File.exist?(File.join(workspace, 'archive', slug)))
  fixture.assert_equal('active', runner.send(:lifecycle_state,
                      File.read(File.join(workspace, 'work', slug, 'state.md'))))
  fixture.refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
  fixture.assert_equal('pending', runner.tracking_sync_receipt(slug).fetch('state'))
else
  abort 'unknown tracking-files fixture phase'
end
