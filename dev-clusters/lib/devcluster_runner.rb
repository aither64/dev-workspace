# frozen_string_literal: true

require 'fileutils'
require 'json'
require 'optparse'
require 'osvm'
require 'time'

module DevClusters
  class OsVmRunner
    ShutdownRequested = Class.new(StandardError)
    MachineState = Struct.new(:name, :machine, keyword_init: true)

    def self.run(argv, hash_base:, priority_machines: [])
      new(argv, hash_base:, priority_machines:).run
    end

    def initialize(argv, hash_base:, priority_machines:)
      @argv = argv
      @hash_base = hash_base
      @priority_machines = priority_machines
    end

    def run
      command = @argv.shift

      case command
      when 'start'
        start
      else
        warn "Usage: #{$PROGRAM_NAME} start --config PATH --state-dir DIR --sock-dir DIR --pid-file PATH --ready-file PATH"
        2
      end
    end

    private

    attr_reader :hash_base, :priority_machines

    def start
      opts = {
        timeout: 900
      }

      OptionParser.new do |parser|
        parser.on('--config PATH') { |v| opts[:config] = v }
        parser.on('--state-dir DIR') { |v| opts[:state_dir] = v }
        parser.on('--sock-dir DIR') { |v| opts[:sock_dir] = v }
        parser.on('--pid-file PATH') { |v| opts[:pid_file] = v }
        parser.on('--ready-file PATH') { |v| opts[:ready_file] = v }
        parser.on('--timeout SECONDS', Integer) { |v| opts[:timeout] = v }
      end.parse!(@argv)

      %i[config state_dir sock_dir pid_file ready_file].each do |key|
        raise ArgumentError, "--#{key.to_s.tr('_', '-')} is required" unless opts[key]
      end

      FileUtils.mkdir_p(opts[:state_dir])
      FileUtils.mkdir_p(opts[:sock_dir])
      FileUtils.mkdir_p(File.dirname(opts[:pid_file]))
      FileUtils.rm_f(opts[:ready_file])
      File.write(opts[:pid_file], "#{Process.pid}\n")

      machines = build_machines(opts)
      stopping = false

      stop_all = proc do
        next if stopping

        stopping = true
        machines.reverse_each do |entry|
          begin
            warn "Stopping #{entry.name}"
            entry.machine.stop(timeout: 120)
          rescue StandardError => e
            warn "Graceful stop failed for #{entry.name}: #{e.class}: #{e.message}"
            begin
              entry.machine.kill(signal: 'TERM')
            rescue StandardError => kill_error
              warn "Kill failed for #{entry.name}: #{kill_error.class}: #{kill_error.message}"
            end
          end
        end
      end

      signal_reader, signal_writer = IO.pipe
      signal_thread = Thread.new do
        begin
          signal_reader.read(1)
          Thread.main.raise ShutdownRequested
        rescue IOError
          # The main thread closes the pipe during normal shutdown.
        end
      end

      signal_trap = proc do
        begin
          signal_writer.write_nonblock('.')
        rescue IO::WaitWritable, IOError, Errno::EPIPE
          # The shutdown thread is already notified or gone.
        end
      end

      Signal.trap('TERM', &signal_trap)
      Signal.trap('INT', &signal_trap)

      begin
        start_machines(machines, opts[:timeout])
        File.write(opts[:ready_file], "#{Time.now.utc.iso8601}\n")

        loop do
          break if stopping

          sleep 2
        end
      rescue ShutdownRequested
        # Normal path for SIGTERM/SIGINT. Shutdown happens in ensure.
      ensure
        Signal.trap('TERM', 'IGNORE')
        Signal.trap('INT', 'IGNORE')
        stop_all.call
        machines.each do |entry|
          begin
            entry.machine.finalize
            entry.machine.cleanup
          rescue StandardError => e
            warn "Cleanup failed for #{entry.name}: #{e.class}: #{e.message}"
          end
        end
        FileUtils.rm_f(opts[:ready_file])
        FileUtils.rm_f(opts[:pid_file])
        signal_writer.close unless signal_writer.closed?
        signal_reader.close unless signal_reader.closed?
        signal_thread.kill
        signal_thread.join
      end

      0
    end

    def build_machines(opts)
      config = JSON.parse(File.read(opts[:config]))

      config.fetch('machines').map do |name, machine_cfg|
        osvm_cfg = OsVm::MachineConfig.from_config(machine_cfg)
        klass = machine_class(osvm_cfg)

        MachineState.new(
          name: name,
          machine: klass.new(
            name,
            osvm_cfg,
            opts[:state_dir],
            opts[:sock_dir],
            default_timeout: opts[:timeout],
            hash_base:
          )
        )
      end
    end

    def machine_class(config)
      case config.spin
      when 'nixos'
        OsVm::NixosMachine
      when 'vpsadminos'
        OsVm::VpsadminosMachine
      else
        raise "Unsupported machine spin #{config.spin.inspect}"
      end
    end

    def start_machines(machines, timeout)
      priority_names = priority_machines.each_with_index.to_h
      priority, rest = machines.partition { |entry| priority_names.key?(entry.name) }
      priority.sort_by! { |entry| priority_names.fetch(entry.name) }

      start_machine_group(priority, timeout)
      start_machine_group(rest, timeout)
    end

    def start_machine_group(machines, timeout)
      machines.each do |entry|
        warn "Starting #{entry.name}"
        entry.machine.start(wait_for_boot: false)
      end

      machines.each do |entry|
        warn "Waiting for #{entry.name}"
        entry.machine.wait_for_boot(timeout:)
      end
    end
  end
end
