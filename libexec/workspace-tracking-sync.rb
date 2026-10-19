# frozen_string_literal: true

module DevSession
  # Local closure is independent of Git publication. A receipt survives a
  # successful sync so archive/revive can prove their local transition offline.
  module TrackingSync
    def defer_tracking?
      @env['DEV_WORKSPACE_DEFER_TRACKING'] == '1'
    end

    def portal_lifecycle_receipt(slug, operation, operation_id)
      workspace_id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      path = File.join(@workspace_state_root, 'portal', workspace_id, 'lifecycle-operations.json')
      receipts = read_private_json_object(path, 'portal lifecycle authorization', max_size: 256 * 1024, mode: 0o600)
      unless [1, 2].include?(receipts['schema']) && receipts['workspace'] == workspace
        raise Error, 'invalid portal lifecycle authorization'
      end
      receipt = Array(receipts['operations']).find do |item|
        item['slug'] == slug && item['kind'] == operation &&
          %w[running paused failed].include?(item['state']) && item.dig('options', 'journalId') == operation_id
      end
      raise Error, 'portal lifecycle authorization is no longer available' unless receipt

      receipt
    end

    def authorized_archive_worker?(slug, operation_id, unit)
      receipt = portal_lifecycle_receipt(slug, 'archive', operation_id)
      unit == "workspace-archive-#{receipt['receiptId']}-#{receipt['attempt']}.service"
    rescue Error
      false
    end

    def tracking_sync_root
      workspace_id = Digest::SHA256.hexdigest(workspace)
      File.join(@workspace_state_root, 'tracking-sync', workspace_id)
    end

    def archive_progress!(slug, stage, path = nil)
      @out.puts "archive: #{stage}#{path ? " #{path}" : ''}"
      return unless @archive_progress_operation_id

      directory = File.join(@workspace_state_root, 'archive-progress', Digest::SHA256.hexdigest(workspace))
      write_transition_json!(File.join(directory, "#{slug}.json"), {
        'schema' => 1, 'workspace' => workspace, 'slug' => slug,
        'operation_id' => @archive_progress_operation_id, 'stage' => stage,
        'path' => path, 'updated_at' => Time.now.utc.iso8601
      })
    rescue Errno::EIO, Errno::EPIPE
      raise unless @archive_retiring_own_terminal
    end

    def tracking_sync_receipt(slug)
      path = File.join(tracking_sync_root, "#{slug}.json")
      return nil unless path_exists?(path)

      value = read_private_json_object(path, 'tracking sync receipt', max_size: 64 * 1024, mode: 0o600)
      unless value['schema'] == 1 && value['workspace'] == workspace && value['slug'] == slug &&
             %w[archive revive].include?(value['direction']) && %w[pending synced].include?(value['state']) &&
             %w[complete abandoned archived].include?(value['mode']) &&
             value['attempts'].is_a?(Integer) && value['attempts'] >= 0 &&
             value['error'].is_a?(String) && value['error'].bytesize <= 16 * 1024 &&
             valid_rfc3339_timestamp?(value['updated_at']) && valid_rfc3339_timestamp?(value['next_attempt_at']) &&
             value['operation_id'].is_a?(String) && value['operation_id'].match?(/\A[0-9a-f]{64}\z/) &&
             value['target_sha256'].is_a?(String) && value['target_sha256'].match?(/\A[0-9a-f]{64}\z/)
        raise Error, 'invalid tracking sync receipt'
      end
      value.to_h
    end

    def local_tracking_journal?(journal)
      journal.fetch('schema') >= (journal.key?('lifecycle') ? 5 : 4)
    end

    def save_tracking_transition!(slug, journal, direction:)
      return validate_sensitive_transition!(slug, direction:) if sensitive_session?(slug)

      previous = tracking_sync_receipt(slug)
      return verify_saved_tracking_transition!(slug, journal, direction:) if previous &&
        previous['operation_id'] == journal.fetch('operation_id')

      write_tracking_sync!(slug, {
        'schema' => 1, 'workspace' => workspace, 'slug' => slug,
        'operation_id' => journal.fetch('operation_id'), 'direction' => direction,
        'mode' => journal['mode'] || journal.fetch('lifecycle'),
        'target_sha256' => journal.fetch('target_tracking_sha256'),
        'state' => 'pending', 'error' => '', 'attempts' => 0,
        'updated_at' => Time.now.utc.iso8601, 'next_attempt_at' => Time.now.utc.iso8601
      })
      verify_saved_tracking_transition!(slug, journal, direction:)
      @out.puts 'tracking sync pending'
    end

    def verify_saved_tracking_transition!(slug, journal, direction:)
      return validate_sensitive_transition!(slug, direction:) if sensitive_session?(slug)

      receipt = tracking_sync_receipt(slug)
      destination = direction == 'archive' ? archive_dir(slug) : work_dir(slug)
      unless receipt && receipt['operation_id'] == journal.fetch('operation_id') &&
             receipt['direction'] == direction && receipt['target_sha256'] == journal.fetch('target_tracking_sha256') &&
             tracking_tree_matches?(destination, receipt.fetch('target_sha256'))
        raise Error, "saved local tracking transition changed: #{slug}"
      end
    end

    def locally_archived_tracking?(slug)
      receipt = tracking_sync_receipt(slug)
      return false unless receipt && receipt['direction'] == 'archive'

      manifest = load_portal_manifest(File.join(archive_dir(slug), PORTAL_MANIFEST), required: true, expected_slug: slug)
      return false if manifest['archive_operation_id'] && manifest['archive_operation_id'] != receipt['operation_id']

      unless tracking_tree_matches?(archive_dir(slug), receipt.fetch('target_sha256'))
        raise Error, "locally archived tracking changed: #{slug}"
      end
      true
    end

    def discard_tracking_sync!(slug)
      path = File.join(tracking_sync_root, "#{slug}.json")
      File.unlink(path) if path_exists?(path)
      fsync_directory(tracking_sync_root) if File.directory?(tracking_sync_root)
    end

    def tracking_sync(slug = nil, force: false)
      slugs = slug ? [slug] : Dir.glob(File.join(tracking_sync_root, '*.json')).map { |path| File.basename(path, '.json') }
      slugs.sort.map do |candidate|
        validate_part!(candidate, 'tracking sync slug')
        begin
          with_tracking_sync_deadline do
            with_creation_lock(candidate) do
              with_slug_lock(candidate) do
                receipt = tracking_sync_receipt(candidate)
                next receipt unless receipt && receipt['state'] == 'pending'
                next receipt if !force && Time.iso8601(receipt.fetch('next_attempt_at')) > Time.now.utc

                reject_conflicting_lifecycle_journal!(candidate)
                begin
                  commit_tracking_transition!(candidate, direction: receipt.fetch('direction'), mode: receipt.fetch('mode'))
                  receipt['state'] = 'synced'
                  receipt['error'] = ''
                rescue Error, CommandError => e
                  receipt['attempts'] += 1
                  receipt['error'] = e.message.encode('UTF-8', invalid: :replace, undef: :replace).each_char.take(4096).join
                  receipt['next_attempt_at'] = (Time.now.utc + [30 * (2**[receipt['attempts'], 6].min), 1800].min).iso8601
                end
                receipt['updated_at'] = Time.now.utc.iso8601
                write_tracking_sync!(candidate, receipt)
                receipt
              end
            end
          end
        rescue Error => e
          { 'slug' => candidate, 'state' => 'pending', 'error' => e.message }
        end
      end
    end

    def tracking_sync_status(slug = nil)
      slugs = slug ? [slug] : Dir.glob(File.join(tracking_sync_root, '*.json')).map { |path| File.basename(path, '.json') }
      slugs.sort.filter_map do |candidate|
        validate_part!(candidate, 'tracking sync slug')
        tracking_sync_receipt(candidate)
      end
    end

    private

    def tracking_sync_attempt_seconds
      60
    end

    def with_tracking_sync_deadline
      previous = @tracking_sync_deadline
      @tracking_sync_deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + tracking_sync_attempt_seconds
      @command_runner.with_deadline(@tracking_sync_deadline) { yield }
    ensure
      @tracking_sync_deadline = previous
    end

    def write_tracking_sync!(slug, receipt)
      write_transition_json!(File.join(tracking_sync_root, "#{slug}.json"), receipt)
    end

    def write_transition_json!(path, value)
      directory = File.dirname(path)
      FileUtils.mkdir_p(directory, mode: 0o700)
      temporary = "#{path}.#{$$}.tmp"
      begin
        File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
          file.write(JSON.generate(value) + "\n")
          file.flush
          file.fsync
        end
        File.rename(temporary, path)
        fsync_directory(directory)
      ensure
        File.unlink(temporary) if File.exist?(temporary)
      end
    end
  end
end
