# frozen_string_literal: true

module DevWorkspaceHost
  module WorkspaceCoordinationClaimsHost
    CLAIM_ACTIONS = %w[acquire release cancel-claim claim-status verify-machine].freeze

    def coordination_claim(entry, action, slug, argv)
      options = {}
      OptionParser.new do |opts|
        opts.on('--as-is') { options[:as_is] = true }
        opts.on('--json') { options[:json] = true }
        opts.on('--request FILE') { |value| options[:request] = value }
        opts.on('--id UUID') { |value| options[:id] = value }
        opts.on('--repository NAME') { |value| options[:repository] = value }
        opts.on('--selector EXACT') { |value| options[:selector] = value }
      end.parse!(argv)
      unless argv.empty? && slug && slug.match?(WorkspaceCoordination::PART) && options[:as_is]
        raise WorkspaceCoordination::Error, 'resource coordination requires exact SESSION --as-is and no extra arguments'
      end
      if action == 'verify-machine'
        unless (options.keys - %i[as_is json id repository selector]).empty? && options[:json] &&
               options[:id]&.match?(WorkspaceCoordination::UUID) && options[:repository]&.match?(WorkspaceCoordination::PART) && options[:selector]
          raise WorkspaceCoordination::Error, 'verify-machine requires exact id, repository, selector and --json'
        end
        WorkspaceCoordination::MachineIdentity.text!(options[:selector])
      elsif options.key?(:repository) || options.key?(:selector)
        raise WorkspaceCoordination::Error, 'repository and selector options apply only to verify-machine'
      end
      engine = coordination_engine(entry)
      if action == 'claim-status'
        owners = engine.store.lifecycle_owners(engine.store.read, engine.workspace, slug)
        records = engine.store.claims['claims'].values.select { |item| owners.any? { |owner| owner['id'] == item['ownerId'] } }
        result = options[:id] ? records.find { |item| item['id'] == options[:id] } || raise(WorkspaceCoordination::Error, 'unknown session claim') : records
      else
        result = nil
        with_coordination_session_admission(entry, slug) do
          engine.current_generation!
          owner = engine.claim_owner(slug, read_only: action == 'verify-machine')
          result = if action == 'acquire'
            engine.acquire_claim(owner, options[:request] ? coordination_input(options[:request]) : raise(WorkspaceCoordination::Error, 'acquire requires --request FILE'))
          elsif action == 'verify-machine'
            engine.verify_machine(owner, options[:id], options[:repository], options[:selector])
          else
            engine.change_claim(owner, options[:id], action == 'release' ? 'release' : 'cancel')
          end
        end
      end
      @out.puts JSON.generate(result)
    end
  end
end
