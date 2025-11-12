import { type Journey } from '@buf/pushpa_cotton.bufbuild_es/journeys/v1/journeys_pb'
import { useAtom } from 'jotai'
import { Plus } from 'lucide-react'
import { useState, useEffect } from 'react'
import JourneyForm from './new'
import { selectedProjectAtom } from '@/atoms/projects'
import { AppSidebar } from '@/components/nav/app-sidebar'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { SidebarProvider, SidebarInset, SidebarTrigger } from '@/components/ui/sidebar'
import { journeysService } from '@/lib/rpc'

function Journeys() {
  const [isDialogOpen, setIsDialogOpen] = useState(false)
  const [journeys, setJourneys] = useState<Journey[]>([])
  const [loading, setLoading] = useState(true)
  const [selectedProjectId] = useAtom(selectedProjectAtom)

  useEffect(() => {
    const fetchJourneys = async () => {
      try {
        setLoading(true)
        const request = { projectId: selectedProjectId }
        const response = await journeysService.list(request)
        setJourneys(response.journeys)
      } catch (error) {
        console.error('Error fetching journeys:', error)
      } finally {
        setLoading(false)
      }
    }

    fetchJourneys()
  }, [selectedProjectId])

  const handleFormSubmitSuccess = () => {
    setIsDialogOpen(false)
    const fetchJourneys = async () => {
      try {
        setLoading(true)
        const request = { projectId: selectedProjectId }
        const response = await journeysService.list(request)
        setJourneys(response.journeys)
      } catch (error) {
        console.error('Error fetching journeys:', error)
      } finally {
        setLoading(false)
      }
    }
    fetchJourneys()
  }

  // Helper function to convert state enum to readable string
  const getStateString = (state: number | undefined) => {
    if (state === undefined) return 'Unknown'
    const stateEnum: Record<number, string> = {
      0: 'Unspecified',
      1: 'Active',
      2: 'Draft',
      3: 'Paused',
      4: 'Archived'
    }
    return stateEnum[state] || 'Unknown'
  }

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
          <SidebarTrigger />
          <div className="flex-1">
            <h1 className="text-xl font-semibold">Journeys</h1>
          </div>
          <Button onClick={() => setIsDialogOpen(true)}>
            <Plus />
            Create
          </Button>
        </header>

        <Dialog open={isDialogOpen} onOpenChange={setIsDialogOpen}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>New Journey</DialogTitle>
            </DialogHeader>
            <JourneyForm
              projectId={selectedProjectId}
              onClose={() => setIsDialogOpen(false)}
              onSubmitSuccess={handleFormSubmitSuccess}
            />
          </DialogContent>
        </Dialog>

        <main className="flex-1 p-4 sm:p-6 lg:p-8">
          <div className="max-w-4xl mx-auto">
            {loading ? (
              <div className="text-center py-8">
                <p>Loading journeys...</p>
              </div>
            ) : (
              <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
                {journeys.map((journey) => (
                  <div key={journey.id} className="border rounded-lg p-4 bg-card">
                    <h3 className="font-semibold">{journey.name}</h3>
                    <p className="text-muted-foreground text-sm">{journey.description}</p>
                    <div className="mt-2 text-xs text-muted-foreground">
                      Status: {getStateString(journey.state)}
                    </div>
                  </div>
                ))}
                {journeys.length === 0 && (
                  <div className="col-span-full text-center py-8 text-muted-foreground">
                    No journeys found. Create your first journey to get started.
                  </div>
                )}
              </div>
            )}
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}

export default Journeys
