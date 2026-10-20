# frozen_string_literal: true

module DevWorkspaceHost
  module WorkspaceCoordinationClaimsHost
    CLAIM_ACTIONS = %w[acquire release cancel-claim claim-status].freeze

    def coordination_claim(entry, action, slug, argv)
      options = {}
      OptionParser.new do |opts|
        opts.on('--as-is') { options[:as_is] = true }
        opts.on('--json') { options[:json] = true }
        opts.on('--request FILE') { |value| options[:request] = value }
        opts.on('--id UUID') { |value| options[:id] = value }
      end.parse!(argv)
      unless argv.empty? && slug && slug.match?(WorkspaceCoordination::PART) && options[:as_is]
        raise WorkspaceCoordination::Error, 'resource coordination requires exact SESSION --as-is and no extra arguments'
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
          owner = engine.claim_owner(slug)
          result = if action == 'acquire'
            engine.acquire_claim(owner, options[:request] ? coordination_input(options[:request]) : raise(WorkspaceCoordination::Error, 'acquire requires --request FILE'))
          else
            engine.change_claim(owner, options[:id], action == 'release' ? 'release' : 'cancel')
          end
        end
      end
      @out.puts JSON.generate(result)
    end
  end
end
