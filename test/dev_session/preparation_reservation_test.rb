# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_preparation_reservation_refuses_unrelated_start_and_fork_before_effects
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-10-03-reserved-session-work'
      preparation_reservation_fixture(runner, workspace, slug)

      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end
      assert_match(/reserved by an accepted portal request/, error.message)
      refute_path_exists(File.join(workspace, 'work', slug))

      error = assert_raises(DevSession::Error) do
        runner.fork('missing-source', slug, as_is: true, json: true)
      end
      assert_match(/reserved by an accepted portal request/, error.message)
      refute_path_exists(File.join(workspace, 'work', slug))
    end
  end

  def test_preparation_reservation_allows_only_exact_receipt_owner
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-10-03-reserved-session-work'
      record, = preparation_reservation_fixture(runner, workspace, slug)
      runner.send(:with_creation_lock, slug) do
        runner.send(:with_slug_lock, slug) do
          error = assert_raises(DevSession::Error) do
            runner.send(:guard_portal_preparation_reservation!, slug, { 'receipt_id' => 'b' * 64 })
          end
          assert_match(/reserved by an accepted portal request/, error.message)
          runner.send(:guard_portal_preparation_reservation!, slug, { 'receipt_id' => record.fetch('receiptId') })
        end
      end
    end
  end

  def test_preparation_reservation_ignores_completed_mappings_but_bounds_unfinished_scan
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-10-03-reserved-session-work'
      record, pending = preparation_reservation_fixture(runner, workspace, slug)
      File.unlink(File.join(pending, "#{record.fetch('requestId')}.json"))
      completed = File.join(File.dirname(pending), 'session-preparation-mappings')
      FileUtils.mkdir_p(completed, mode: 0o700)
      File.write(File.join(completed, 'not-part-of-the-cli-scan'), 'not JSON')
      runner.send(:guard_portal_preparation_reservation!, slug, nil)

      513.times do |number|
        id = format('00000000-0000-4000-8000-%012d', number)
        File.write(File.join(pending, "#{id}.json"), '{}')
      end
      error = assert_raises(DevSession::Error) { runner.send(:guard_portal_preparation_reservation!, slug, nil) }
      assert_match(/too many unfinished/, error.message)
    end
  end

  def test_preparation_reservation_uses_the_existing_cross_process_creation_lock
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-10-03-lock-contention'
      lock_path = runner.send(:session_lock_file, slug, kind: 'creation')
      ready_reader, ready_writer = IO.pipe
      release_reader, release_writer = IO.pipe
      pid = Process.fork do
        ready_reader.close
        release_writer.close
        File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |file|
          file.flock(File::LOCK_EX)
          ready_writer.write('1')
          ready_writer.close
          release_reader.read(1)
        end
        exit! 0
      end
      ready_writer.close
      release_reader.close
      assert_equal('1', ready_reader.read(1))
      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end
      assert_match(/another dev-session command/, error.message)
      refute_path_exists(File.join(workspace, 'work', slug))
    ensure
      release_writer&.write('1')
      release_writer&.close
      ready_reader&.close
      Process.wait(pid) if pid
    end
  end

  def test_preparation_reservation_survives_paused_failed_and_handoff_recovery
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-10-03-reserved-session-work'
      record, pending = preparation_reservation_fixture(runner, workspace, slug)
      path = File.join(pending, "#{record.fetch('requestId')}.json")
      %w[paused failed handed_off].each do |state|
        record['state'] = state
        File.write(path, JSON.generate(record))
        assert_raises(DevSession::Error) { runner.send(:guard_portal_preparation_reservation!, slug, nil) }
        runner.send(:guard_portal_preparation_reservation!, slug, { 'receipt_id' => record.fetch('receiptId') })
      end
      record['state'] = 'terminal'
      File.write(path, JSON.generate(record))
      runner.send(:guard_portal_preparation_reservation!, slug, nil)
    end
  end

  def test_preparation_reservation_receipt_handoff_remains_owned_without_full_record
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-10-03-reserved-session-work'
      record, pending = preparation_reservation_fixture(runner, workspace, slug)
      File.unlink(File.join(pending, "#{record.fetch('requestId')}.json"))
      receipts = File.join(File.dirname(pending), 'creations')
      FileUtils.mkdir_p(receipts, mode: 0o700)
      path = File.join(receipts, "#{slug}.json")
      receipt = { 'schema' => 1, 'workspace' => workspace, 'request' => { 'slug' => slug },
                  'receiptId' => record.fetch('receiptId'), 'state' => 'paused' }
      File.write(path, JSON.generate(receipt))
      File.chmod(0o600, path)
      assert_raises(DevSession::Error) { runner.send(:guard_portal_preparation_reservation!, slug, nil) }
      runner.send(:guard_portal_preparation_reservation!, slug, { 'receipt_id' => record.fetch('receiptId') })
      receipt['state'] = 'ready'
      File.write(path, JSON.generate(receipt))
      runner.send(:guard_portal_preparation_reservation!, slug, nil)
    end
  end

  def test_preparation_reservation_rejects_malformed_private_records_before_effects
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-10-03-reserved-session-work'
      record, pending = preparation_reservation_fixture(runner, workspace, slug)
      path = File.join(pending, "#{record.fetch('requestId')}.json")
      [record.merge('workspace' => '/another-workspace'), record.merge('handoff' => 'wrong-shape')].each do |invalid|
        File.write(path, JSON.generate(invalid))
        assert_raises(DevSession::Error) { runner.send(:guard_portal_preparation_reservation!, slug, nil) }
      end
      File.write(path, JSON.generate(record))
      File.chmod(0o644, path)
      assert_raises(DevSession::Error) { runner.send(:guard_portal_preparation_reservation!, slug, nil) }
      File.chmod(0o600, path)
      target = File.join(File.dirname(pending), 'private-record.json')
      File.rename(path, target)
      File.symlink(target, path)
      assert_raises(DevSession::Error) { runner.send(:guard_portal_preparation_reservation!, slug, nil) }
      refute_path_exists(File.join(workspace, 'work', slug))
    end
  end

  private

  def preparation_reservation_fixture(runner, workspace, slug)
    workspace_id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
    root = File.join(runner.instance_variable_get(:@workspace_state_root), 'portal', workspace_id, 'session-preparations')
    FileUtils.mkdir_p(root, mode: 0o700)
    record = {
      'schema' => 1, 'workspace' => workspace,
      'requestId' => '00000000-0000-4000-8000-000000000001',
      'inputVersion' => 1, 'inputDigest' => 'c' * 64,
      'receiptId' => 'a' * 64, 'state' => 'running',
      'slug' => slug, 'epoch' => 'd' * 64, 'handoff' => { 'slug' => slug }
    }
    path = File.join(root, "#{record.fetch('requestId')}.json")
    File.write(path, JSON.generate(record))
    File.chmod(0o600, path)
    [record, root]
  end
end
