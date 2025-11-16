import type { Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { CircleCheck } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'

interface VapidProps {
  project: Project | null
}

const Vapid = (props: VapidProps) => {
  const { project } = props

  const isProjectAvailable = project !== undefined && project !== null

  const [showConfiguration, setShowConfiguration] = useState(false)
  const [isConfigured, setIsConfigured] = useState(false)

  const handleConfigure = () => {
    // This would normally make an API call to configure VAPID
    toast.success('VAPID keys configured successfully!')
    setIsConfigured(true)
    setShowConfiguration(false)
  }

  return (
    <>
      <Card className="p-4">
        <div className="flex items-center justify-between">
          <CardTitle className="text-lg">Web Push (VAPID)</CardTitle>
          {isConfigured && (
            <div className="flex items-center">
              <div className="flex items-center justify-center w-6 h-6 rounded-full bg-green-500/20">
                <CircleCheck className="h-4 w-4 text-green-600" />
              </div>
            </div>
          )}
        </div>
        <CardDescription className="mt-2 text-sm">
          Set up VAPID keys for web push notifications
        </CardDescription>
        <Button
          variant="outline"
          className="mt-4"
          onClick={() => setShowConfiguration(true)}
          disabled={!isProjectAvailable}
        >
          {isConfigured ? 'Edit Configuration' : 'Configure'}
        </Button>
      </Card>

      <Dialog open={showConfiguration} onOpenChange={setShowConfiguration}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Configure VAPID Keys</DialogTitle>
            <DialogDescription>
              Enter your VAPID public and private keys
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4">
            <div className="space-y-2">
              <label htmlFor="publicKey" className="text-sm font-medium">Public Key</label>
              <textarea 
                id="publicKey" 
                rows={4}
                className="w-full p-2 border rounded font-mono text-sm" 
                placeholder="Paste your VAPID public key" 
              />
            </div>
            <div className="space-y-2">
              <label htmlFor="privateKey" className="text-sm font-medium">Private Key</label>
              <textarea 
                id="privateKey" 
                rows={4}
                className="w-full p-2 border rounded font-mono text-sm" 
                placeholder="Paste your VAPID private key" 
              />
            </div>
            <div className="flex justify-end gap-2 pt-2">
              <Button
                variant="outline"
                onClick={() => setShowConfiguration(false)}
              >
                Cancel
              </Button>
              <Button onClick={handleConfigure}>
                Save Configuration
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default Vapid